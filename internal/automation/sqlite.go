package automation

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	E "github.com/sagernet/sing/common/exceptions"
)

// The SQLite calls live in sqlite_darwin.go (the system libsqlite3 through cgo)
// and sqlite_other.go (modernc.org/sqlite), behind sqliteConn and sqliteStmt.

type DatabaseChange struct {
	Swiplet  string
	Database string
	Path     string
	Table    string
	RowID    int64
}

type databaseWatcher struct {
	swiplet string
	publish func(DatabaseChange)
}

type databaseStore struct {
	access    sync.Mutex
	next      int
	databases map[int]*database
	watchers  map[*databaseWatcher]struct{}
}

type database struct {
	access     sync.Mutex
	handle     int
	client     any
	swiplet    string
	directory  string
	path       string
	file       string
	watch      bool
	conn       *sqliteConn
	next       int
	statements map[int]sqliteStmt
}

type databaseRequest struct {
	Op           string          `json:"op"`
	Handle       int             `json:"handle"`
	Statement    int             `json:"statement"`
	Path         string          `json:"path"`
	Changes      bool            `json:"changes"`
	SQL          string          `json:"sql"`
	Params       json.RawMessage `json:"params"`
	Count        int             `json:"count"`
	Schema       string          `json:"schema"`
	Source       int             `json:"source"`
	SourceSchema string          `json:"sourceSchema"`
	Data         []byte          `json:"data"`
	Asset        string          `json:"asset"`
	Overwrite    bool            `json:"overwrite"`
}

type databaseRun struct {
	Changes         int64    `json:"changes"`
	LastInsertRowID int64    `json:"lastInsertRowId"`
	Columns         []string `json:"columns"`
	Rows            [][]any  `json:"rows"`
	Done            bool     `json:"done"`
}

type databaseChangeEvent struct {
	DatabaseName     string `json:"databaseName"`
	DatabaseFilePath string `json:"databaseFilePath"`
	TableName        string `json:"tableName"`
	RowID            int64  `json:"rowId"`
}

type databaseChangeBatch struct {
	access sync.Mutex
	taken  bool
	events []databaseChangeEvent
}

type databaseRows struct {
	Rows [][]any `json:"rows"`
	Done bool    `json:"done"`
}

const (
	memoryDatabase = ":memory:"
	statementLimit = 256
	readBudget     = 1 << 20
)

var (
	errDatabaseClosed = E.Cause1(ErrInvalid, E.New("the database is closed"))
	errNoStatement    = E.Cause1(ErrInvalid, E.New("the SQL has no statement"))
	memorySequence    atomic.Uint64
)

func newDatabaseStore() databaseStore {
	return databaseStore{databases: make(map[int]*database), watchers: make(map[*databaseWatcher]struct{})}
}

func sqliteOperation(_ context.Context, call *operation) (any, error) {
	if len(call.args) != 1 {
		return nil, E.Cause1(ErrInvalid, E.New("takes 1 argument, not ", len(call.args)))
	}
	return call.engine.host.SQLite(call.engine, call.engine.dataOwner(), call.args[0])
}

func (h *Host) SQLite(client any, swiplet string, request []byte) (any, error) {
	var call databaseRequest
	if err := json.Unmarshal(request, &call); err != nil {
		return nil, E.Cause1(ErrInvalid, E.Cause(err, "decode the database request"))
	}
	switch call.Op {
	case "open":
		return h.openDatabase(client, swiplet, call)
	case "deserialize":
		return h.deserializeDatabase(client, swiplet, call)
	case "delete":
		return nil, h.deleteDatabase(swiplet, call.Path)
	case "import":
		return nil, h.importDatabase(swiplet, call)
	case "interrupt":
		h.databases.access.Lock()
		defer h.databases.access.Unlock()
		db, found := h.databases.databases[call.Handle]
		if !found || db.client != client || db.swiplet != swiplet {
			return nil, errDatabaseClosed
		}
		db.conn.interrupt()
		return nil, nil
	}
	db, err := h.database(client, swiplet, call.Handle, call.Op == "close")
	if err != nil {
		return nil, err
	}
	switch call.Op {
	case "close":
		db.close()
		return nil, nil
	case "connect":
		return h.connectDatabase(db)
	case "backup":
		source, err := h.database(client, swiplet, call.Source, false)
		if err != nil {
			return nil, err
		}
		return nil, backupDatabase(db, schemaName(call.Schema), source, schemaName(call.SourceSchema))
	}
	db.access.Lock()
	result, err := db.serve(call)
	changes := db.takeChanges()
	db.access.Unlock()
	h.publishChanges(swiplet, changes)
	return result, err
}

func (h *Host) database(client any, swiplet string, handle int, remove bool) (*database, error) {
	h.databases.access.Lock()
	defer h.databases.access.Unlock()
	db, found := h.databases.databases[handle]
	if !found || db.client != client || db.swiplet != swiplet {
		return nil, errDatabaseClosed
	}
	if remove {
		delete(h.databases.databases, handle)
	}
	return db, nil
}

func schemaName(name string) string {
	if name == "" {
		return "main"
	}
	return name
}

func (h *Host) dataDirectory(swiplet string) (string, error) {
	if swiplet == REPLDirectory {
		return filepath.Join(h.options.Directory, REPLDirectory, DataFolder), nil
	}
	found, err := h.Swiplet(swiplet)
	if err != nil {
		return "", err
	}
	return filepath.Join(found.Directory, DataFolder), nil
}

func (h *Host) databasePath(swiplet, name string) (string, error) {
	if name == memoryDatabase {
		return name, nil
	}
	cleaned, err := cleanName(name)
	if err != nil {
		return "", err
	}
	if cleaned == "." {
		return "", E.Cause1(ErrInvalid, E.New("the database needs a file name"))
	}
	directory, err := h.dataDirectory(swiplet)
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, cleaned), nil
}

func (h *Host) openDatabase(client any, swiplet string, call databaseRequest) (any, error) {
	directory, err := h.dataDirectory(swiplet)
	if err != nil {
		return nil, err
	}
	path, err := h.databasePath(swiplet, call.Path)
	if err != nil {
		return nil, err
	}
	file := path
	if path == memoryDatabase {
		file = memoryFile()
	} else if err = os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := openConnection(client, swiplet, directory, path, file, call.Changes)
	if err != nil {
		return nil, E.Cause(err, "open ", strconv.Quote(call.Path))
	}
	return h.register(db), nil
}

func (h *Host) connectDatabase(db *database) (any, error) {
	db.access.Lock()
	defer db.access.Unlock()
	if db.conn == nil {
		return nil, errDatabaseClosed
	}
	connection, err := openConnection(db.client, db.swiplet, db.directory, db.path, db.file, db.watch)
	if err != nil {
		return nil, E.Cause(err, "open a new connection")
	}
	return h.register(connection), nil
}

func (h *Host) deserializeDatabase(client any, swiplet string, call databaseRequest) (any, error) {
	directory, err := h.dataDirectory(swiplet)
	if err != nil {
		return nil, err
	}
	db, err := openConnection(client, swiplet, directory, memoryDatabase, memoryFile(), call.Changes)
	if err != nil {
		return nil, E.Cause(err, "deserialize the database")
	}
	if len(call.Data) > 19 && call.Data[18] == 2 && call.Data[19] == 2 {
		call.Data[18], call.Data[19] = 1, 1
	}
	if len(call.Data) > 0 {
		if err = db.restore(call.Data); err != nil {
			db.close()
			return nil, E.Cause(err, "deserialize the database")
		}
	}
	return h.register(db), nil
}

func memoryFile() string {
	return "file:/swipt-" + strconv.FormatUint(memorySequence.Add(1), 10) + "?vfs=memdb"
}

func openConnection(client any, swiplet, directory, path, file string, watch bool) (*database, error) {
	conn, err := openSQLite(file, path == memoryDatabase)
	if err != nil {
		return nil, err
	}
	db := &database{client: client, swiplet: swiplet, directory: directory, path: path, file: file, watch: watch, conn: conn, statements: make(map[int]sqliteStmt)}
	conn.confine(directory)
	conn.setWait()
	if path != memoryDatabase {
		if err = conn.exec("PRAGMA journal_mode = WAL"); err != nil {
			db.close()
			return nil, err
		}
	}
	if watch {
		conn.watchChanges()
	}
	return db, nil
}

func (h *Host) register(db *database) any {
	h.databases.access.Lock()
	h.databases.next++
	db.handle = h.databases.next
	h.databases.databases[db.handle] = db
	h.databases.access.Unlock()
	return map[string]any{"handle": db.handle, "path": db.path}
}

func (db *database) restore(data []byte) error {
	scratch, err := openSQLite(memoryDatabase, false)
	if err != nil {
		return err
	}
	defer scratch.close()
	if err = scratch.deserialize(data); err != nil {
		return err
	}
	return copyDatabase(db.conn, "main", scratch, "main")
}

func backupDatabase(dest *database, destSchema string, source *database, sourceSchema string) error {
	first, second := dest, source
	if second.handle < first.handle {
		first, second = second, first
	}
	first.access.Lock()
	defer first.access.Unlock()
	if second != first {
		second.access.Lock()
		defer second.access.Unlock()
	}
	if dest.conn == nil || source.conn == nil {
		return errDatabaseClosed
	}
	return copyDatabase(dest.conn, destSchema, source.conn, sourceSchema)
}

func (h *Host) deleteDatabase(swiplet, name string) error {
	if name == memoryDatabase {
		return E.Cause1(ErrInvalid, E.New("a memory database cannot be deleted"))
	}
	path, err := h.databasePath(swiplet, name)
	if err != nil {
		return err
	}
	h.databases.access.Lock()
	for _, db := range h.databases.databases {
		if db.path == path {
			h.databases.access.Unlock()
			return E.Cause1(ErrInvalid, E.New("the database ", strconv.Quote(name), " is open"))
		}
	}
	h.databases.access.Unlock()
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if removeErr := os.Remove(path + suffix); !errors.Is(removeErr, os.ErrNotExist) {
			err = E.Errors(err, removeErr)
		}
	}
	return err
}

func (h *Host) importDatabase(swiplet string, call databaseRequest) error {
	if call.Path == memoryDatabase {
		return E.Cause1(ErrInvalid, E.New("a memory database cannot be imported"))
	}
	path, err := h.databasePath(swiplet, call.Path)
	if err != nil {
		return err
	}
	source, err := h.assetPath(swiplet, call.Asset)
	if err != nil {
		return err
	}
	if _, err = os.Stat(path); err == nil && !call.Overwrite {
		return nil
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	staging, err := os.CreateTemp(filepath.Dir(path), ".import-")
	if err != nil {
		return err
	}
	defer os.Remove(staging.Name())
	err = copyFile(staging, source)
	if closeErr := staging.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return E.Cause(err, "copy ", strconv.Quote(call.Asset))
	}
	if err = h.deleteDatabase(swiplet, call.Path); err != nil {
		return err
	}
	return os.Rename(staging.Name(), path)
}

func (h *Host) assetPath(swiplet, asset string) (string, error) {
	found, err := h.Swiplet(swiplet)
	if err != nil {
		return "", err
	}
	directory := filepath.Join(found.Directory, AssetsFolder)
	if !strings.HasPrefix(asset, "file://") {
		cleaned, err := cleanName(asset)
		if err != nil {
			return "", err
		}
		return filepath.Join(directory, cleaned), nil
	}
	location, err := url.Parse(asset)
	if err != nil {
		return "", E.Cause1(ErrInvalid, E.Cause(err, "parse ", strconv.Quote(asset)))
	}
	directory, err = filepath.EvalSymlinks(directory)
	if err != nil {
		return "", E.Cause1(ErrNotFound, E.New("the swiplet has no assets"))
	}
	resolved, err := filepath.EvalSymlinks(location.Path)
	if err != nil {
		return "", E.Cause1(ErrNotFound, E.New("there is no asset ", strconv.Quote(asset)))
	}
	if relative, err := filepath.Rel(directory, resolved); err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", E.Cause1(ErrNotAllowed, E.New(strconv.Quote(asset), " is not an asset of the swiplet"))
	}
	return resolved, nil
}

func copyFile(target *os.File, source string) error {
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(target, file)
	return err
}

func (h *Host) watchDatabaseChanges(ctx context.Context, swiplet string, publish func(any, error)) {
	var access sync.Mutex
	var batch *databaseChangeBatch
	h.WatchDatabases(ctx, swiplet, func(change DatabaseChange) {
		event := databaseChangeEvent{DatabaseName: change.Database, DatabaseFilePath: change.Path, TableName: change.Table, RowID: change.RowID}
		access.Lock()
		defer access.Unlock()
		if batch != nil && batch.add(event) {
			return
		}
		batch = &databaseChangeBatch{events: []databaseChangeEvent{event}}
		publish(batch, nil)
	})
}

func (b *databaseChangeBatch) add(event databaseChangeEvent) bool {
	b.access.Lock()
	defer b.access.Unlock()
	if !b.taken {
		b.events = append(b.events, event)
	}
	return !b.taken
}

func (b *databaseChangeBatch) MarshalJSON() ([]byte, error) {
	b.access.Lock()
	b.taken = true
	events := b.events
	b.events = nil
	b.access.Unlock()
	return json.Marshal(events)
}

func (h *Host) CloseDatabases(client any, swiplet string) {
	h.databases.access.Lock()
	var closing []*database
	for handle, db := range h.databases.databases {
		if (client == nil || db.client == client) && (swiplet == "" || db.swiplet == swiplet) {
			closing = append(closing, db)
			delete(h.databases.databases, handle)
		}
	}
	h.databases.access.Unlock()
	for _, db := range closing {
		db.close()
	}
}

func (h *Host) WatchDatabases(ctx context.Context, swiplet string, publish func(DatabaseChange)) {
	watcher := &databaseWatcher{swiplet: swiplet, publish: publish}
	h.databases.access.Lock()
	h.databases.watchers[watcher] = struct{}{}
	h.databases.access.Unlock()
	<-ctx.Done()
	h.databases.access.Lock()
	delete(h.databases.watchers, watcher)
	h.databases.access.Unlock()
}

func (h *Host) publishChanges(swiplet string, changes []DatabaseChange) {
	if len(changes) == 0 {
		return
	}
	h.databases.access.Lock()
	var watchers []*databaseWatcher
	for watcher := range h.databases.watchers {
		if watcher.swiplet == "" || watcher.swiplet == swiplet {
			watchers = append(watchers, watcher)
		}
	}
	h.databases.access.Unlock()
	for _, watcher := range watchers {
		for _, change := range changes {
			watcher.publish(change)
		}
	}
}

func (db *database) close() {
	db.access.Lock()
	defer db.access.Unlock()
	if db.conn == nil {
		return
	}
	for _, stmt := range db.statements {
		db.conn.finalize(stmt)
	}
	clear(db.statements)
	db.conn.close()
	db.conn = nil
}

func (db *database) serve(call databaseRequest) (any, error) {
	if db.conn == nil {
		return nil, errDatabaseClosed
	}
	switch call.Op {
	case "exec":
		return nil, db.conn.exec(call.SQL)
	case "prepare":
		return db.prepare(call.SQL)
	case "query":
		stmt, err := db.compile(call.SQL, false)
		if err != nil {
			return nil, err
		}
		defer db.conn.finalize(stmt)
		return db.run(stmt, call.Params, call.Count)
	case "finalize":
		if stmt, found := db.statements[call.Statement]; found {
			delete(db.statements, call.Statement)
			db.conn.finalize(stmt)
		}
		return nil, nil
	case "run", "step", "reset":
		stmt, found := db.statements[call.Statement]
		if !found {
			return nil, E.Cause1(ErrInvalid, E.New("the statement is finalized"))
		}
		switch call.Op {
		case "run":
			return db.run(stmt, call.Params, call.Count)
		case "step":
			rows, done, err := db.read(stmt, call.Count)
			if err != nil {
				return nil, err
			}
			return databaseRows{Rows: rows, Done: done}, nil
		}
		db.conn.reset(stmt)
		return nil, nil
	case "autocommit":
		return db.conn.autocommit(), nil
	case "serialize":
		data, ok := db.conn.serialize(schemaName(call.Schema))
		if !ok {
			return nil, E.Cause1(ErrInvalid, E.New("the database ", strconv.Quote(schemaName(call.Schema)), " cannot be serialized"))
		}
		return map[string][]byte{"data": data}, nil
	}
	return nil, E.Cause1(ErrInvalid, E.New("unknown database operation ", strconv.Quote(call.Op)))
}

func (db *database) compile(source string, persistent bool) (stmt sqliteStmt, err error) {
	if source == "" {
		return stmt, errNoStatement
	}
	return db.conn.prepare(source, persistent)
}

func (db *database) prepare(source string) (any, error) {
	stmt, err := db.compile(source, true)
	if err != nil {
		return nil, err
	}
	if len(db.statements) >= statementLimit {
		handles := slices.Sorted(maps.Keys(db.statements))
		evicted := handles[0]
		if index := slices.IndexFunc(handles, func(handle int) bool { return !db.conn.busy(db.statements[handle]) }); index >= 0 {
			evicted = handles[index]
		}
		db.conn.finalize(db.statements[evicted])
		delete(db.statements, evicted)
	}
	db.next++
	db.statements[db.next] = stmt
	return map[string]any{"statement": db.next, "columns": db.columnNames(stmt)}, nil
}

func (db *database) run(stmt sqliteStmt, params json.RawMessage, count int) (any, error) {
	db.conn.reset(stmt)
	db.conn.clearBindings(stmt)
	if err := db.bind(stmt, params); err != nil {
		return nil, err
	}
	rows, done, err := db.read(stmt, count)
	if err != nil {
		return nil, err
	}
	return databaseRun{
		Changes:         db.conn.changeCount(),
		LastInsertRowID: db.conn.lastInsertRowID(),
		Columns:         db.columnNames(stmt),
		Rows:            rows,
		Done:            done,
	}, nil
}

func (db *database) read(stmt sqliteStmt, count int) ([][]any, bool, error) {
	rows := [][]any{}
	size := 0
	for count == 0 || (len(rows) < count && size < readBudget) {
		found, err := db.conn.step(stmt)
		if err != nil {
			db.conn.reset(stmt)
			return nil, true, err
		}
		if !found {
			db.conn.reset(stmt)
			return rows, true, nil
		}
		row := make([]any, db.conn.columnCount(stmt))
		for index := range row {
			value, bytes := db.conn.column(stmt, index)
			row[index] = encodeColumn(value)
			size += bytes
		}
		rows = append(rows, row)
	}
	return rows, false, nil
}

func (db *database) bind(stmt sqliteStmt, encoded json.RawMessage) error {
	if len(encoded) == 0 || string(encoded) == "null" {
		return nil
	}
	var list []json.RawMessage
	if json.Unmarshal(encoded, &list) == nil {
		for index, item := range list {
			if err := db.bindValue(stmt, index+1, item); err != nil {
				return err
			}
		}
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &object); err != nil {
		return E.Cause1(ErrInvalid, E.New("the parameters must be an array or an object"))
	}
	for key, item := range object {
		index := db.parameterIndex(stmt, key)
		if index == 0 {
			continue
		}
		if err := db.bindValue(stmt, index, item); err != nil {
			return err
		}
	}
	return nil
}

func (db *database) parameterIndex(stmt sqliteStmt, key string) int {
	names := []string{key}
	if key == "" || !strings.ContainsRune("$:@?", rune(key[0])) {
		names = []string{"$" + key, ":" + key, "@" + key}
	}
	for _, name := range names {
		if index := db.conn.parameterIndex(stmt, name); index != 0 {
			return index
		}
	}
	return 0
}

// bindValue binds an encoded parameter as nil, int64, float64, string or []byte.
func (db *database) bindValue(stmt sqliteStmt, index int, item json.RawMessage) error {
	decoder := json.NewDecoder(strings.NewReader(string(item)))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return E.Cause1(ErrInvalid, E.Cause(err, "decode a parameter"))
	}
	var bound any
	switch v := value.(type) {
	case nil:
	case bool:
		var integer int64
		if v {
			integer = 1
		}
		bound = integer
	case string:
		bound = v
	case json.Number:
		if integer, err := v.Int64(); err == nil {
			bound = integer
		} else if float, err := v.Float64(); err == nil {
			bound = float
		} else {
			return E.Cause1(ErrInvalid, E.New("the number ", v.String(), " is out of range"))
		}
	case map[string]any:
		if encoded, ok := v["$b"].(string); ok && len(v) == 1 {
			data, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				return E.Cause1(ErrInvalid, E.Cause(err, "decode a blob parameter"))
			}
			bound = data
		} else if decimal, ok := v["$i"].(string); ok && len(v) == 1 {
			integer, err := strconv.ParseInt(decimal, 10, 64)
			if err != nil {
				return E.Cause1(ErrInvalid, E.New("the bigint ", decimal, " is out of the 64-bit range"))
			}
			bound = integer
		} else {
			return E.Cause1(ErrInvalid, E.New("a parameter must be null, a number, a bigint, a string, a boolean or a Uint8Array"))
		}
	default:
		return E.Cause1(ErrInvalid, E.New("a parameter must be null, a number, a bigint, a string, a boolean or a Uint8Array"))
	}
	return db.conn.bind(stmt, index, bound)
}

func (db *database) takeChanges() []DatabaseChange {
	if db.conn == nil {
		return nil
	}
	changes := db.conn.takeChanges()
	for index := range changes {
		changes[index].Swiplet, changes[index].Path = db.swiplet, db.path
	}
	return changes
}

func (db *database) columnNames(stmt sqliteStmt) []string {
	names := make([]string, db.conn.columnCount(stmt))
	for index := range names {
		names[index] = db.conn.columnName(stmt, index)
	}
	return names
}

// encodeColumn turns a column read as nil, int64, float64, string or []byte
// into its JSON form.
func encodeColumn(value any) any {
	switch v := value.(type) {
	case float64:
		if math.IsInf(v, 0) {
			return nil
		}
	case []byte:
		return map[string]string{"$b": base64.StdEncoding.EncodeToString(v)}
	}
	return value
}
