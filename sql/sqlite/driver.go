// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/adler32"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/veiloq/atlas/sql/internal/sqlx"
	"github.com/veiloq/atlas/sql/migrate"
	"github.com/veiloq/atlas/sql/schema"
	"github.com/veiloq/atlas/sql/sqlclient"
)

type (
	// Driver represents a SQLite driver for introspecting database schemas,
	// generating diff between schema elements and apply migrations changes.
	Driver struct {
		*conn
		schema.Differ
		schema.Inspector
		migrate.PlanApplier
	}

	// database connection and its information.
	conn struct {
		schema.ExecQuerier
		url *sqlclient.URL
	}
)

var _ interface {
	migrate.StmtScanner
	schema.TypeParseFormatter
} = (*Driver)(nil)

// DriverName holds the name used for registration.
const DriverName = "sqlite3"

func init() {
	sqlclient.Register(
		DriverName,
		sqlclient.OpenerFunc(opener),
		sqlclient.RegisterDriverOpener(Open),
		sqlclient.RegisterTxOpener(OpenTx),
		sqlclient.RegisterCodec(codec, codec),
		sqlclient.RegisterFlavours("sqlite"),
		sqlclient.RegisterURLParser(urlparse{}),
	)
	sqlclient.Register(
		"libsql",
		sqlclient.DriverOpener(Open),
		sqlclient.RegisterTxOpener(OpenTx),
		sqlclient.RegisterCodec(codec, codec),
		sqlclient.RegisterFlavours("libsql+ws", "libsql+wss", "libsql+file"),
		sqlclient.RegisterURLParser(sqlclient.URLParserFunc(func(u *url.URL) *sqlclient.URL {
			dsn := strings.TrimPrefix(u.String(), "libsql+")
			if strings.HasPrefix(dsn, "file://") {
				dsn = strings.Replace(dsn, "file://", "file:", 1)
			}
			return &sqlclient.URL{URL: u, DSN: dsn, Schema: mainFile}
		})),
	)
}

type urlparse struct{}

// ParseURL implements the sqlclient.URLParser interface.
func (urlparse) ParseURL(u *url.URL) *sqlclient.URL {
	uc := &sqlclient.URL{URL: u, DSN: strings.TrimPrefix(u.String(), u.Scheme+"://"), Schema: mainFile}
	if mode := u.Query().Get("mode"); mode == "memory" {
		// The "file:" prefix is mandatory for memory modes.
		uc.DSN = "file:" + uc.DSN
	}
	return uc
}

func opener(_ context.Context, u *url.URL) (*sqlclient.Client, error) {
	ur := urlparse{}.ParseURL(u)
	db, err := sql.Open(DriverName, ur.DSN)
	if err != nil {
		return nil, err
	}
	drv, err := Open(db)
	if err != nil {
		if cerr := db.Close(); cerr != nil {
			err = fmt.Errorf("%w: %v", err, cerr)
		}
		return nil, err
	}
	if drv, ok := drv.(*Driver); ok {
		drv.url = ur
	}
	return &sqlclient.Client{
		Name:   DriverName,
		DB:     db,
		URL:    ur,
		Driver: drv,
	}, nil
}

// Open opens a new SQLite driver.
func Open(db schema.ExecQuerier) (migrate.Driver, error) {
	c := &conn{ExecQuerier: db}
	return &Driver{
		conn:        c,
		Differ:      &sqlx.Diff{DiffDriver: &diff{}},
		Inspector:   &inspect{c},
		PlanApplier: &planApply{c},
	}, nil
}

// Snapshot implements migrate.Snapshoter.
func (d *Driver) Snapshot(ctx context.Context) (migrate.RestoreFunc, error) {
	r, err := d.InspectRealm(ctx, nil)
	if err != nil {
		return nil, err
	}
	if !(r == nil || (len(r.Schemas) == 1 && r.Schemas[0].Name == mainFile && len(r.Schemas[0].Tables) == 0)) {
		return nil, &migrate.NotCleanError{State: r, Reason: fmt.Sprintf("found table %q", r.Schemas[0].Tables[0].Name)}
	}
	return func(ctx context.Context) error {
		for _, stmt := range []string{
			"PRAGMA writable_schema = 1;",
			"DELETE FROM sqlite_master WHERE type IN ('table', 'view', 'index', 'trigger');",
			"PRAGMA writable_schema = 0;",
			"VACUUM;",
		} {
			if _, err := d.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	}, nil
}

// CheckClean implements migrate.CleanChecker.
func (d *Driver) CheckClean(ctx context.Context, revT *migrate.TableIdent) error {
	r, err := d.InspectRealm(ctx, nil)
	if err != nil {
		return err
	}
	switch n := len(r.Schemas); {
	case n > 1:
		return &migrate.NotCleanError{State: r, Reason: fmt.Sprintf("found multiple schemas: %d", len(r.Schemas))}
	case n == 1 && r.Schemas[0].Name != mainFile:
		return &migrate.NotCleanError{State: r, Reason: fmt.Sprintf("found schema %q", r.Schemas[0].Name)}
	case n == 1 && len(r.Schemas[0].Tables) > 1:
		return &migrate.NotCleanError{State: r, Reason: fmt.Sprintf("found multiple tables: %d", len(r.Schemas[0].Tables))}
	case n == 1 && len(r.Schemas[0].Tables) == 1 && (revT == nil || r.Schemas[0].Tables[0].Name != revT.Name):
		return &migrate.NotCleanError{State: r, Reason: fmt.Sprintf("found table %q", r.Schemas[0].Tables[0].Name)}
	}
	return nil
}

// Lock implements the schema.Locker interface.
func (d *Driver) Lock(_ context.Context, name string, timeout time.Duration) (schema.UnlockFunc, error) {
	// If the URL was set and the database is a file, use its name in the lock file.
	if d.url != nil && strings.HasPrefix(d.url.DSN, "file:") {
		p := filepath.Join(d.url.Host, d.url.Path)
		name = fmt.Sprintf("%s_%s", name, fmt.Sprintf("%x", adler32.Checksum([]byte(p))))
	}
	path := filepath.Join(os.TempDir(), name+".lock")
	c, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return acquireLock(path, timeout)
	}
	if err != nil {
		return nil, fmt.Errorf("sql/sqlite: reading lock dir: %w", err)
	}
	expires, err := strconv.ParseInt(string(c), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("sql/sqlite: invalid lock file format: parsing expiration date: %w", err)
	}
	if time.Unix(0, expires).After(time.Now()) {
		// Lock is still valid.
		return nil, fmt.Errorf("sql/sqlite: lock on %q already taken", name)
	}
	return acquireLock(path, timeout)
}

// FormatType converts schema type to its column form in the database.
func (*Driver) FormatType(t schema.Type) (string, error) {
	return FormatType(t)
}

// ParseType returns the schema.Type value represented by the given string.
func (*Driver) ParseType(s string) (schema.Type, error) {
	return ParseType(s)
}

// StmtBuilder is a helper method used to build statements with SQLite formatting.
func (*Driver) StmtBuilder(opts migrate.PlanOptions) *sqlx.Builder {
	return &sqlx.Builder{
		QuoteOpening: '`',
		QuoteClosing: '`',
		Schema:       opts.SchemaQualifier,
		Indent:       opts.Indent,
	}
}

// ScanStmts implements migrate.StmtScanner.
func (*Driver) ScanStmts(input string) ([]*migrate.Stmt, error) {
	return (&migrate.Scanner{
		ScannerOptions: migrate.ScannerOptions{
			MatchBegin: true,
			// The following are not support by SQLite.
			MatchBeginAtomic: false,
			MatchDollarQuote: false,
		},
	}).Scan(input)
}

func acquireLock(path string, timeout time.Duration) (schema.UnlockFunc, error) {
	lock, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("sql/sqlite: creating lockfile %q: %w", path, err)
	}
	if _, err := lock.Write([]byte(strconv.FormatInt(time.Now().Add(timeout).UnixNano(), 10))); err != nil {
		return nil, fmt.Errorf("sql/sqlite: writing to lockfile %q: %w", path, err)
	}
	defer lock.Close()
	return func() error { return os.Remove(path) }, nil
}

type violation struct {
	tbl, ref   string
	row, index int
}

// OpenTx opens a transaction. If foreign keys are enabled, it disables them, checks for constraint violations,
// opens the transaction and before committing ensures no new violations have been introduced by whatever Atlas was
// doing.
func OpenTx(ctx context.Context, db *sql.DB, opts *sql.TxOptions) (*sqlclient.Tx, error) {
	var on sql.NullBool
	if err := db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&on); err != nil {
		return nil, fmt.Errorf("sql/sqlite: querying 'foreign_keys' pragma: %w", err)
	}
	// Disable the foreign_keys pragma in case it is enabled, and
	// toggle it back after transaction is committed or rolled back.
	if on.Bool {
		_, err := db.ExecContext(ctx, "PRAGMA foreign_keys = off")
		if err != nil {
			return nil, fmt.Errorf("sql/sqlite: set 'foreign_keys = off': %w", err)
		}
	}
	tx, err := db.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	cm, err := CommitFunc(ctx, db, tx, on.Bool)
	if err != nil {
		return nil, err
	}
	return &sqlclient.Tx{
		Tx:         tx,
		CommitFn:   cm,
		RollbackFn: RollbackFunc(ctx, db, tx, on.Bool),
	}, nil
}

// Tx wraps schema.ExecQuerier with the transaction methods.
type Tx interface {
	schema.ExecQuerier
	Commit() error
	Rollback() error
}

// CommitFunc takes a transaction and ensures to toggle foreign keys back on after tx.Commit is called.
func CommitFunc(ctx context.Context, db schema.ExecQuerier, tx Tx, on bool) (func() error, error) {
	var (
		before []violation
		err    error
	)
	if on {
		before, err = violations(ctx, tx)
		if err != nil {
			return nil, err
		}
	}
	return func() error {
		if on {
			after, err := violations(ctx, tx)
			if err != nil {
				if err2 := tx.Rollback(); err2 != nil {
					err = fmt.Errorf("%v: %w", err2, err)
				}
				return enableFK(ctx, db, on, err)
			}
			if vs := violationsDiff(before, after); len(vs) > 0 {
				err := fmt.Errorf("sql/sqlite: foreign key mismatch: %+v", vs)
				if err2 := tx.Rollback(); err2 != nil {
					err = fmt.Errorf("%v: %w", err2, err)
				}
				return enableFK(ctx, db, on, err)
			}
		}
		return enableFK(ctx, db, on, tx.Commit())
	}, nil
}

// RollbackFunc takes a transaction and ensures to toggle foreign keys back on after tx.Rollback is called.
func RollbackFunc(ctx context.Context, db schema.ExecQuerier, tx Tx, on bool) func() error {
	return func() error {
		return enableFK(ctx, db, on, tx.Rollback())
	}
}

func enableFK(ctx context.Context, db schema.ExecQuerier, do bool, err error) error {
	if do {
		// Re-enable foreign key checks if they were enabled before.
		if _, err2 := db.ExecContext(ctx, "PRAGMA foreign_keys = on"); err2 != nil {
			err2 = fmt.Errorf("sql/sqlite: set 'foreign_keys = on': %w", err2)
			if err != nil {
				return fmt.Errorf("%v: %w", err2, err)
			}
			return err2
		}
	}
	return err
}

func violations(ctx context.Context, conn schema.ExecQuerier) ([]violation, error) {
	rows, err := conn.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return nil, fmt.Errorf("sql/sqlite: querying 'foreign_key_check' pragma: %w", err)
	}
	defer rows.Close()
	var vs []violation
	for rows.Next() {
		var v violation
		if err := rows.Scan(&v.tbl, &v.row, &v.ref, &v.index); err != nil {
			return nil, fmt.Errorf("sql/sqlite: querying 'foreign_key_check' pragma: scanning rows: %w", err)
		}
		vs = append(vs, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sql/sqlite: querying 'foreign_key_check' pragma: scanning rows: %w", err)
	}
	return vs, nil
}

// equalViolations compares the foreign key violations before starting a transaction with the ones afterwards.
// It returns violations found in v2 that are not in v1.
func violationsDiff(v1, v2 []violation) (vs []violation) {
	for _, v := range v2 {
		if !contains(v1, v) {
			vs = append(vs, v)
		}
	}
	return vs
}

func contains(hs []violation, n violation) bool {
	for _, v := range hs {
		if v.row == n.row && v.ref == n.ref && v.index == n.index && v.tbl == n.tbl {
			return true
		}
	}
	return false
}

// SQLite standard data types as defined in its codebase and documentation.
// https://www.sqlite.org/datatype3.html
// https://github.com/sqlite/sqlite/blob/master/src/global.c
const (
	TypeInteger = "integer" // SQLITE_TYPE_INTEGER
	TypeReal    = "real"    // SQLITE_TYPE_REAL
	TypeText    = "text"    // SQLITE_TYPE_TEXT
	TypeBlob    = "blob"    // SQLITE_TYPE_BLOB
)

// SQLite generated columns types.
const (
	virtual = "VIRTUAL"
	stored  = "STORED"
)
