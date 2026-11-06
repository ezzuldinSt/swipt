//go:build darwin && cgo

package automation

/*
#cgo LDFLAGS: -lsqlite3

#include <sqlite3.h>
#include <stdlib.h>
#include <string.h>

typedef struct {
	char *schema;
	char *table;
	sqlite3_int64 row;
} swipt_change;

typedef struct {
	swipt_change *items;
	size_t count;
	size_t capacity;
} swipt_changes;

static void swipt_record_change(void *context, int operation, const char *schema, const char *table, sqlite3_int64 row) {
	swipt_changes *changes = context;
	if (changes->count == changes->capacity) {
		size_t capacity = changes->capacity == 0 ? 16 : changes->capacity * 2;
		swipt_change *items = realloc(changes->items, capacity * sizeof(swipt_change));
		if (items == NULL) {
			return;
		}
		changes->items = items;
		changes->capacity = capacity;
	}
	changes->items[changes->count++] = (swipt_change){strdup(schema), strdup(table), row};
}

static void swipt_clear_changes(void *context) {
	swipt_changes *changes = context;
	for (size_t index = 0; index < changes->count; index++) {
		free(changes->items[index].schema);
		free(changes->items[index].table);
	}
	changes->count = 0;
}

static void swipt_watch_changes(sqlite3 *db, swipt_changes *changes) {
	sqlite3_update_hook(db, swipt_record_change, changes);
	sqlite3_rollback_hook(db, swipt_clear_changes, changes);
}

static void swipt_free_changes(swipt_changes *changes) {
	swipt_clear_changes(changes);
	free(changes->items);
	free(changes);
}

static int swipt_interrupted(sqlite3 *db) {
	if (__builtin_available(iOS 17.2, macOS 14.2, *)) {
		return sqlite3_is_interrupted(db);
	}
	return 0;
}

static int swipt_wait(void *context, int count) {
	static const int delays[] = {1, 2, 5, 10, 15, 20, 25, 25, 25, 50, 50, 100};
	static const int steps = sizeof(delays) / sizeof(delays[0]);
	int waited = 0;
	for (int index = 0; index < count; index++) {
		waited += delays[index < steps ? index : steps - 1];
	}
	if (waited >= 5000 || swipt_interrupted(context)) {
		return 0;
	}
	sqlite3_sleep(delays[count < steps ? count : steps - 1]);
	return !swipt_interrupted(context);
}

static void swipt_set_wait(sqlite3 *db) {
	sqlite3_busy_handler(db, swipt_wait, db);
}

static int swipt_bind_text(sqlite3_stmt *statement, int index, const char *text, int length) {
	return sqlite3_bind_text(statement, index, length == 0 ? "" : text, length, SQLITE_TRANSIENT);
}

static int swipt_deserialize(sqlite3 *db, const void *data, sqlite3_int64 size) {
	unsigned char *buffer = sqlite3_malloc64(size);
	if (buffer == NULL) {
		return SQLITE_NOMEM;
	}
	memcpy(buffer, data, size);
	return sqlite3_deserialize(db, "main", buffer, size, size, SQLITE_DESERIALIZE_FREEONCLOSE | SQLITE_DESERIALIZE_RESIZEABLE);
}

static int swipt_inside(const char *directory, const char *file) {
	size_t length = strlen(directory);
	if (strncmp(file, directory, length) != 0 || file[length] != '/') {
		return 0;
	}
	for (const char *segment = file + length; *segment != '\0';) {
		segment++;
		size_t size = strcspn(segment, "/");
		if ((size == 1 && segment[0] == '.') || (size == 2 && segment[0] == '.' && segment[1] == '.')) {
			return 0;
		}
		segment += size;
	}
	return 1;
}

static int swipt_authorize(void *directory, int action, const char *file, const char *unused, const char *schema, const char *trigger) {
	if (action != SQLITE_ATTACH || (file != NULL && (file[0] == '\0' || strcmp(file, ":memory:") == 0))) {
		return SQLITE_OK;
	}
	return file != NULL && sqlite3_strnicmp(file, "file:", 5) != 0 && swipt_inside(directory, file) ? SQLITE_OK : SQLITE_DENY;
}

static void swipt_confine(sqlite3 *db, const char *directory) {
	sqlite3_set_authorizer(db, swipt_authorize, (void *)directory);
}

static int swipt_bind_blob(sqlite3_stmt *statement, int index, const void *data, int length) {
	if (length == 0) {
		return sqlite3_bind_zeroblob(statement, index, 0);
	}
	return sqlite3_bind_blob(statement, index, data, length, SQLITE_TRANSIENT);
}
*/
import "C"

import (
	"bytes"
	"unsafe"

	E "github.com/sagernet/sing/common/exceptions"
)

type sqliteConn struct {
	db       *C.sqlite3
	confined *C.char
	changes  *C.swipt_changes
}

type sqliteStmt = *C.sqlite3_stmt

func openSQLite(file string, uri bool) (*sqliteConn, error) {
	flags := C.int(C.SQLITE_OPEN_READWRITE | C.SQLITE_OPEN_CREATE | C.SQLITE_OPEN_NOMUTEX)
	if uri {
		flags |= C.SQLITE_OPEN_URI
	}
	name := C.CString(file)
	defer C.free(unsafe.Pointer(name))
	var db *C.sqlite3
	if code := C.sqlite3_open_v2(name, &db, flags, nil); code != C.SQLITE_OK {
		err := sqliteFailure(db, code)
		C.sqlite3_close_v2(db)
		return nil, err
	}
	return &sqliteConn{db: db}, nil
}

func (c *sqliteConn) confine(directory string) {
	c.confined = C.CString(directory)
	C.swipt_confine(c.db, c.confined)
}

func (c *sqliteConn) setWait() {
	C.swipt_set_wait(c.db)
}

func (c *sqliteConn) watchChanges() {
	c.changes = (*C.swipt_changes)(C.calloc(1, C.sizeof_swipt_changes))
	C.swipt_watch_changes(c.db, c.changes)
}

// takeChanges returns the changes recorded since the last call, leaving out
// Swiplet and Path, unless a write transaction is still open.
func (c *sqliteConn) takeChanges() []DatabaseChange {
	if c.changes == nil || c.changes.count == 0 || C.sqlite3_txn_state(c.db, nil) == C.SQLITE_TXN_WRITE {
		return nil
	}
	items := unsafe.Slice(c.changes.items, c.changes.count)
	changes := make([]DatabaseChange, len(items))
	for index, item := range items {
		changes[index] = DatabaseChange{Database: C.GoString(item.schema), Table: C.GoString(item.table), RowID: int64(item.row)}
	}
	C.swipt_clear_changes(unsafe.Pointer(c.changes))
	return changes
}

func (c *sqliteConn) interrupt() {
	C.sqlite3_interrupt(c.db)
}

func (c *sqliteConn) close() {
	C.sqlite3_close_v2(c.db)
	c.db = nil
	if c.confined != nil {
		C.free(unsafe.Pointer(c.confined))
		c.confined = nil
	}
	if c.changes != nil {
		C.swipt_free_changes(c.changes)
		c.changes = nil
	}
}

func sqliteFailure(conn *C.sqlite3, code C.int) error {
	message := C.GoString(C.sqlite3_errstr(code))
	if conn != nil && C.sqlite3_errcode(conn) != C.SQLITE_OK {
		message = C.GoString(C.sqlite3_errmsg(conn))
	}
	return E.Cause1(ErrInvalid, E.New(message))
}

func (c *sqliteConn) exec(source string) error {
	text := C.CString(source)
	defer C.free(unsafe.Pointer(text))
	if code := C.sqlite3_exec(c.db, text, nil, nil, nil); code != C.SQLITE_OK {
		return sqliteFailure(c.db, code)
	}
	return nil
}

func (c *sqliteConn) prepare(source string, persistent bool) (sqliteStmt, error) {
	var flags C.uint
	if persistent {
		flags = C.SQLITE_PREPARE_PERSISTENT
	}
	var stmt *C.sqlite3_stmt
	code := C.sqlite3_prepare_v3(c.db, (*C.char)(unsafe.Pointer(unsafe.StringData(source))), C.int(len(source)), flags, &stmt, nil)
	if code != C.SQLITE_OK {
		return nil, sqliteFailure(c.db, code)
	}
	if stmt == nil {
		return nil, errNoStatement
	}
	return stmt, nil
}

func (c *sqliteConn) finalize(stmt sqliteStmt) {
	C.sqlite3_finalize(stmt)
}

func (c *sqliteConn) reset(stmt sqliteStmt) {
	C.sqlite3_reset(stmt)
}

func (c *sqliteConn) clearBindings(stmt sqliteStmt) {
	C.sqlite3_clear_bindings(stmt)
}

func (c *sqliteConn) busy(stmt sqliteStmt) bool {
	return C.sqlite3_stmt_busy(stmt) != 0
}

func (c *sqliteConn) step(stmt sqliteStmt) (bool, error) {
	switch code := C.sqlite3_step(stmt); code {
	case C.SQLITE_ROW:
		return true, nil
	case C.SQLITE_DONE:
		return false, nil
	default:
		return false, sqliteFailure(c.db, code)
	}
}

func (c *sqliteConn) autocommit() bool {
	return C.sqlite3_get_autocommit(c.db) != 0
}

func (c *sqliteConn) changeCount() int64 {
	return int64(C.sqlite3_changes64(c.db))
}

func (c *sqliteConn) lastInsertRowID() int64 {
	return int64(C.sqlite3_last_insert_rowid(c.db))
}

func (c *sqliteConn) serialize(schema string) ([]byte, bool) {
	name := C.CString(schema)
	defer C.free(unsafe.Pointer(name))
	var size C.sqlite3_int64
	data := C.sqlite3_serialize(c.db, name, &size, 0)
	if data == nil {
		return nil, false
	}
	defer C.sqlite3_free(unsafe.Pointer(data))
	return bytes.Clone(unsafe.Slice((*byte)(unsafe.Pointer(data)), int(size))), true
}

func (c *sqliteConn) deserialize(data []byte) error {
	if code := C.swipt_deserialize(c.db, unsafe.Pointer(unsafe.SliceData(data)), C.sqlite3_int64(len(data))); code != C.SQLITE_OK {
		return sqliteFailure(c.db, code)
	}
	return nil
}

func copyDatabase(dest *sqliteConn, destSchema string, source *sqliteConn, sourceSchema string) error {
	destName := C.CString(destSchema)
	defer C.free(unsafe.Pointer(destName))
	sourceName := C.CString(sourceSchema)
	defer C.free(unsafe.Pointer(sourceName))
	backup := C.sqlite3_backup_init(dest.db, destName, source.db, sourceName)
	if backup == nil {
		return sqliteFailure(dest.db, C.sqlite3_errcode(dest.db))
	}
	code := C.sqlite3_backup_step(backup, -1)
	if finished := C.sqlite3_backup_finish(backup); finished != C.SQLITE_OK {
		return sqliteFailure(dest.db, finished)
	}
	if code != C.SQLITE_DONE {
		return sqliteFailure(nil, code)
	}
	return nil
}

func (c *sqliteConn) parameterIndex(stmt sqliteStmt, name string) int {
	text := C.CString(name)
	defer C.free(unsafe.Pointer(text))
	return int(C.sqlite3_bind_parameter_index(stmt, text))
}

func (c *sqliteConn) bind(stmt sqliteStmt, index int, value any) error {
	var code C.int
	switch v := value.(type) {
	case nil:
		code = C.sqlite3_bind_null(stmt, C.int(index))
	case int64:
		code = C.sqlite3_bind_int64(stmt, C.int(index), C.sqlite3_int64(v))
	case float64:
		code = C.sqlite3_bind_double(stmt, C.int(index), C.double(v))
	case string:
		code = C.swipt_bind_text(stmt, C.int(index), (*C.char)(unsafe.Pointer(unsafe.StringData(v))), C.int(len(v)))
	case []byte:
		code = C.swipt_bind_blob(stmt, C.int(index), unsafe.Pointer(unsafe.SliceData(v)), C.int(len(v)))
	}
	if code != C.SQLITE_OK {
		return sqliteFailure(c.db, code)
	}
	return nil
}

func (c *sqliteConn) columnCount(stmt sqliteStmt) int {
	return int(C.sqlite3_column_count(stmt))
}

func (c *sqliteConn) columnName(stmt sqliteStmt, index int) string {
	return C.GoString(C.sqlite3_column_name(stmt, C.int(index)))
}

// column reads a column as nil, int64, float64, string or []byte, with its
// size in bytes.
func (c *sqliteConn) column(stmt sqliteStmt, index int) (any, int) {
	column := C.int(index)
	var value any
	switch C.sqlite3_column_type(stmt, column) {
	case C.SQLITE_INTEGER:
		value = int64(C.sqlite3_column_int64(stmt, column))
	case C.SQLITE_FLOAT:
		value = float64(C.sqlite3_column_double(stmt, column))
	case C.SQLITE_TEXT:
		text := C.sqlite3_column_text(stmt, column)
		value = C.GoStringN((*C.char)(unsafe.Pointer(text)), C.sqlite3_column_bytes(stmt, column))
	case C.SQLITE_BLOB:
		data := C.sqlite3_column_blob(stmt, column)
		value = C.GoBytes(data, C.sqlite3_column_bytes(stmt, column))
	}
	return value, int(C.sqlite3_column_bytes(stmt, column))
}
