//go:build !darwin || !cgo

package automation

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	E "github.com/sagernet/sing/common/exceptions"

	"modernc.org/libc"
	lib "modernc.org/sqlite/lib"
)

// Without cgo the SQLite calls go to modernc.org/sqlite, SQLite translated to
// Go. Every call takes a TLS, which must not be used by two goroutines at once:
// each connection owns one and uses it under the database's lock.
type sqliteConn struct {
	tls       *libc.TLS
	db        uintptr
	hook      uintptr
	directory string
	changes   []DatabaseChange
}

type sqliteStmt = uintptr

const pointerSize = int(unsafe.Sizeof(uintptr(0)))

// sqliteHooks maps the context the authorizer and the change hooks get back
// to their connection, as SQLite cannot hold a Go pointer.
var sqliteHooks = struct {
	access sync.RWMutex
	next   uintptr
	conns  map[uintptr]*sqliteConn
}{conns: make(map[uintptr]*sqliteConn)}

var sqliteWaitDelays = [...]int32{1, 2, 5, 10, 15, 20, 25, 25, 25, 50, 50, 100}

func openSQLite(file string, uri bool) (*sqliteConn, error) {
	flags := int32(lib.SQLITE_OPEN_READWRITE | lib.SQLITE_OPEN_CREATE | lib.SQLITE_OPEN_NOMUTEX)
	if uri {
		flags |= lib.SQLITE_OPEN_URI
	}
	c := &sqliteConn{tls: libc.NewTLS()}
	name := cCopy(c.tls, file)
	handle := c.tls.Alloc(pointerSize)
	code := lib.Xsqlite3_open_v2(c.tls, name, handle, flags, 0)
	c.db = *(*uintptr)(cPointer(handle))
	c.tls.Free(pointerSize)
	libc.Xfree(c.tls, name)
	if code != lib.SQLITE_OK {
		err := c.failure(code)
		c.close()
		return nil, err
	}
	return c, nil
}

func (c *sqliteConn) confine(directory string) {
	c.directory = directory
	lib.Xsqlite3_set_authorizer(c.tls, c.db, cFunction(sqliteAuthorize), c.hooked())
}

func (c *sqliteConn) setWait() {
	lib.Xsqlite3_busy_handler(c.tls, c.db, cFunction(sqliteWait), c.db)
}

func (c *sqliteConn) watchChanges() {
	hook := c.hooked()
	lib.Xsqlite3_update_hook(c.tls, c.db, cFunction(sqliteRecordChange), hook)
	lib.Xsqlite3_rollback_hook(c.tls, c.db, cFunction(sqliteClearChanges), hook)
}

func (c *sqliteConn) hooked() uintptr {
	if c.hook == 0 {
		sqliteHooks.access.Lock()
		sqliteHooks.next++
		c.hook = sqliteHooks.next
		sqliteHooks.conns[c.hook] = c
		sqliteHooks.access.Unlock()
	}
	return c.hook
}

func hookedConn(hook uintptr) *sqliteConn {
	sqliteHooks.access.RLock()
	defer sqliteHooks.access.RUnlock()
	return sqliteHooks.conns[hook]
}

// takeChanges returns the changes recorded since the last call, leaving out
// Swiplet and Path, unless a write transaction is still open.
func (c *sqliteConn) takeChanges() []DatabaseChange {
	if len(c.changes) == 0 || lib.Xsqlite3_txn_state(c.tls, c.db, 0) == lib.SQLITE_TXN_WRITE {
		return nil
	}
	changes := c.changes
	c.changes = nil
	return changes
}

// interrupt runs without the database's lock, so it cannot use the TLS of the
// connection, which a running statement may hold.
func (c *sqliteConn) interrupt() {
	tls := libc.NewTLS()
	defer tls.Close()
	lib.Xsqlite3_interrupt(tls, c.db)
}

func (c *sqliteConn) close() {
	lib.Xsqlite3_close_v2(c.tls, c.db)
	c.db = 0
	if c.hook != 0 {
		sqliteHooks.access.Lock()
		delete(sqliteHooks.conns, c.hook)
		sqliteHooks.access.Unlock()
		c.hook = 0
	}
	c.tls.Close()
}

func sqliteFailure(tls *libc.TLS, db uintptr, code int32) error {
	message := libc.GoString(lib.Xsqlite3_errstr(tls, code))
	if db != 0 && lib.Xsqlite3_errcode(tls, db) != lib.SQLITE_OK {
		message = libc.GoString(lib.Xsqlite3_errmsg(tls, db))
	}
	return E.Cause1(ErrInvalid, E.New(message))
}

func (c *sqliteConn) failure(code int32) error {
	return sqliteFailure(c.tls, c.db, code)
}

func (c *sqliteConn) exec(source string) error {
	text := cCopy(c.tls, source)
	defer libc.Xfree(c.tls, text)
	if code := lib.Xsqlite3_exec(c.tls, c.db, text, 0, 0, 0); code != lib.SQLITE_OK {
		return c.failure(code)
	}
	return nil
}

func (c *sqliteConn) prepare(source string, persistent bool) (sqliteStmt, error) {
	var flags uint32
	if persistent {
		flags = lib.SQLITE_PREPARE_PERSISTENT
	}
	text := cCopy(c.tls, source)
	defer libc.Xfree(c.tls, text)
	handle := c.tls.Alloc(pointerSize)
	defer c.tls.Free(pointerSize)
	if code := lib.Xsqlite3_prepare_v3(c.tls, c.db, text, int32(len(source)), flags, handle, 0); code != lib.SQLITE_OK {
		return 0, c.failure(code)
	}
	stmt := *(*uintptr)(cPointer(handle))
	if stmt == 0 {
		return 0, errNoStatement
	}
	return stmt, nil
}

func (c *sqliteConn) finalize(stmt sqliteStmt) {
	lib.Xsqlite3_finalize(c.tls, stmt)
}

func (c *sqliteConn) reset(stmt sqliteStmt) {
	lib.Xsqlite3_reset(c.tls, stmt)
}

func (c *sqliteConn) clearBindings(stmt sqliteStmt) {
	lib.Xsqlite3_clear_bindings(c.tls, stmt)
}

func (c *sqliteConn) busy(stmt sqliteStmt) bool {
	return lib.Xsqlite3_stmt_busy(c.tls, stmt) != 0
}

func (c *sqliteConn) step(stmt sqliteStmt) (bool, error) {
	switch code := lib.Xsqlite3_step(c.tls, stmt); code {
	case lib.SQLITE_ROW:
		return true, nil
	case lib.SQLITE_DONE:
		return false, nil
	default:
		return false, c.failure(code)
	}
}

func (c *sqliteConn) autocommit() bool {
	return lib.Xsqlite3_get_autocommit(c.tls, c.db) != 0
}

func (c *sqliteConn) changeCount() int64 {
	return lib.Xsqlite3_changes64(c.tls, c.db)
}

func (c *sqliteConn) lastInsertRowID() int64 {
	return lib.Xsqlite3_last_insert_rowid(c.tls, c.db)
}

func (c *sqliteConn) serialize(schema string) ([]byte, bool) {
	name := cCopy(c.tls, schema)
	defer libc.Xfree(c.tls, name)
	size := c.tls.Alloc(8)
	defer c.tls.Free(8)
	data := lib.Xsqlite3_serialize(c.tls, c.db, name, size, 0)
	if data == 0 {
		return nil, false
	}
	defer lib.Xsqlite3_free(c.tls, data)
	return bytes.Clone(cBytes(data, *(*int64)(cPointer(size)))), true
}

func (c *sqliteConn) deserialize(data []byte) error {
	buffer := lib.Xsqlite3_malloc64(c.tls, uint64(len(data)))
	if buffer == 0 {
		return c.failure(lib.SQLITE_NOMEM)
	}
	copy(cBytes(buffer, len(data)), data)
	schema := cCopy(c.tls, "main")
	defer libc.Xfree(c.tls, schema)
	code := lib.Xsqlite3_deserialize(c.tls, c.db, schema, buffer, int64(len(data)), int64(len(data)), lib.SQLITE_DESERIALIZE_FREEONCLOSE|lib.SQLITE_DESERIALIZE_RESIZEABLE)
	if code != lib.SQLITE_OK {
		return c.failure(code)
	}
	return nil
}

// copyDatabase runs on the TLS of dest; the caller holds the locks of both.
func copyDatabase(dest *sqliteConn, destSchema string, source *sqliteConn, sourceSchema string) error {
	tls := dest.tls
	destName := cCopy(tls, destSchema)
	defer libc.Xfree(tls, destName)
	sourceName := cCopy(tls, sourceSchema)
	defer libc.Xfree(tls, sourceName)
	backup := lib.Xsqlite3_backup_init(tls, dest.db, destName, source.db, sourceName)
	if backup == 0 {
		return dest.failure(lib.Xsqlite3_errcode(tls, dest.db))
	}
	code := lib.Xsqlite3_backup_step(tls, backup, -1)
	if finished := lib.Xsqlite3_backup_finish(tls, backup); finished != lib.SQLITE_OK {
		return dest.failure(finished)
	}
	if code != lib.SQLITE_DONE {
		return sqliteFailure(tls, 0, code)
	}
	return nil
}

func (c *sqliteConn) parameterIndex(stmt sqliteStmt, name string) int {
	text := cCopy(c.tls, name)
	defer libc.Xfree(c.tls, text)
	return int(lib.Xsqlite3_bind_parameter_index(c.tls, stmt, text))
}

func (c *sqliteConn) bind(stmt sqliteStmt, index int, value any) error {
	column := int32(index)
	var code int32
	switch v := value.(type) {
	case nil:
		code = lib.Xsqlite3_bind_null(c.tls, stmt, column)
	case int64:
		code = lib.Xsqlite3_bind_int64(c.tls, stmt, column, v)
	case float64:
		code = lib.Xsqlite3_bind_double(c.tls, stmt, column, v)
	case string:
		text := cCopy(c.tls, v)
		code = lib.Xsqlite3_bind_text(c.tls, stmt, column, text, int32(len(v)), lib.SQLITE_TRANSIENT)
		libc.Xfree(c.tls, text)
	case []byte:
		if len(v) == 0 {
			code = lib.Xsqlite3_bind_zeroblob(c.tls, stmt, column, 0)
			break
		}
		data := cCopy(c.tls, v)
		code = lib.Xsqlite3_bind_blob(c.tls, stmt, column, data, int32(len(v)), lib.SQLITE_TRANSIENT)
		libc.Xfree(c.tls, data)
	}
	if code != lib.SQLITE_OK {
		return c.failure(code)
	}
	return nil
}

func (c *sqliteConn) columnCount(stmt sqliteStmt) int {
	return int(lib.Xsqlite3_column_count(c.tls, stmt))
}

func (c *sqliteConn) columnName(stmt sqliteStmt, index int) string {
	return libc.GoString(lib.Xsqlite3_column_name(c.tls, stmt, int32(index)))
}

// column reads a column as nil, int64, float64, string or []byte, with its
// size in bytes.
func (c *sqliteConn) column(stmt sqliteStmt, index int) (any, int) {
	column := int32(index)
	var value any
	switch lib.Xsqlite3_column_type(c.tls, stmt, column) {
	case lib.SQLITE_INTEGER:
		value = lib.Xsqlite3_column_int64(c.tls, stmt, column)
	case lib.SQLITE_FLOAT:
		value = lib.Xsqlite3_column_double(c.tls, stmt, column)
	case lib.SQLITE_TEXT:
		text := lib.Xsqlite3_column_text(c.tls, stmt, column)
		value = string(cBytes(text, lib.Xsqlite3_column_bytes(c.tls, stmt, column)))
	case lib.SQLITE_BLOB:
		data := lib.Xsqlite3_column_blob(c.tls, stmt, column)
		value = bytes.Clone(cBytes(data, lib.Xsqlite3_column_bytes(c.tls, stmt, column)))
	}
	return value, int(lib.Xsqlite3_column_bytes(c.tls, stmt, column))
}

// sqliteAuthorize keeps ATTACH to the data directory, like swipt_authorize in
// sqlite_darwin.go.
func sqliteAuthorize(_ *libc.TLS, hook uintptr, action int32, file, _, _, _ uintptr) int32 {
	if action != lib.SQLITE_ATTACH {
		return lib.SQLITE_OK
	}
	if file == 0 {
		return lib.SQLITE_DENY
	}
	name := libc.GoString(file)
	if name == "" || name == memoryDatabase {
		return lib.SQLITE_OK
	}
	if strings.EqualFold(name[:min(len(name), 5)], "file:") {
		return lib.SQLITE_DENY
	}
	if c := hookedConn(hook); c == nil || !insideDirectory(c.directory, name) {
		return lib.SQLITE_DENY
	}
	return lib.SQLITE_OK
}

// insideDirectory reports whether file names a path below directory without
// "." or ".." segments, like swipt_inside in sqlite_darwin.go but splitting at
// every separator of the platform.
func insideDirectory(directory, file string) bool {
	rest, found := strings.CutPrefix(file, directory)
	if !found || rest == "" || !os.IsPathSeparator(rest[0]) {
		return false
	}
	for segment := range strings.FieldsFuncSeq(rest, func(r rune) bool { return r < 0x80 && os.IsPathSeparator(uint8(r)) }) {
		if segment == "." || segment == ".." {
			return false
		}
	}
	// Windows resolves more than the segments show (trailing dots and spaces,
	// device names), so the full path must stay inside too.
	resolvedDirectory, err := filepath.Abs(directory)
	if err != nil {
		return false
	}
	resolved, err := filepath.Abs(file)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(resolvedDirectory, resolved)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func sqliteRecordChange(_ *libc.TLS, hook uintptr, _ int32, schema, table uintptr, row int64) {
	if c := hookedConn(hook); c != nil {
		c.changes = append(c.changes, DatabaseChange{Database: libc.GoString(schema), Table: libc.GoString(table), RowID: row})
	}
}

func sqliteClearChanges(_ *libc.TLS, hook uintptr) {
	if c := hookedConn(hook); c != nil {
		c.changes = nil
	}
}

// sqliteWait is swipt_wait of sqlite_darwin.go: SQLite's default busy
// schedule for up to 5 seconds, giving up once the connection is interrupted.
func sqliteWait(tls *libc.TLS, db uintptr, count int32) int32 {
	last := int32(len(sqliteWaitDelays) - 1)
	var waited int32
	for index := range count {
		waited += sqliteWaitDelays[min(index, last)]
	}
	if waited >= 5000 || lib.Xsqlite3_is_interrupted(tls, db) != 0 {
		return 0
	}
	lib.Xsqlite3_sleep(tls, sqliteWaitDelays[min(count, last)])
	if lib.Xsqlite3_is_interrupted(tls, db) != 0 {
		return 0
	}
	return 1
}

// cFunction turns a function declaration into the pointer SQLite calls, as
// modernc.org/sqlite's cFuncPointer does.
func cFunction[T any](function T) uintptr {
	return *(*uintptr)(unsafe.Pointer(&struct{ f T }{function}))
}

// cPointer turns an address in C memory, which the Go collector neither moves
// nor frees, into a pointer.
func cPointer(address uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&address))
}

func cBytes[N int | int32 | int64](address uintptr, size N) []byte {
	if address == 0 || size <= 0 {
		return []byte{}
	}
	return unsafe.Slice((*byte)(cPointer(address)), size)
}

// cCopy copies data into C memory with a NUL after it, and like C.CString
// gives up on the process when that memory cannot be had.
func cCopy[T ~string | ~[]byte](tls *libc.TLS, data T) uintptr {
	address := libc.Xmalloc(tls, libc.Tsize_t(len(data)+1))
	if address == 0 {
		panic("sqlite: out of memory")
	}
	memory := cBytes(address, len(data)+1)
	memory[copy(memory, data)] = 0
	return address
}
