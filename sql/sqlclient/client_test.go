// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package sqlclient_test

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"testing"

	"github.com/veiloq/atlas/sql/migrate"
	"github.com/veiloq/atlas/sql/schema"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"

	"github.com/veiloq/atlas/sql/sqlclient"
)

func TestRegisterOpen(t *testing.T) {
	c := &sqlclient.Client{}
	sqlclient.Register(
		"mysql",
		sqlclient.OpenerFunc(func(context.Context, *url.URL) (*sqlclient.Client, error) {
			return c, nil
		}),
		sqlclient.RegisterFlavours("maria"),
		sqlclient.RegisterURLParser(sqlclient.URLParserFunc(func(u *url.URL) *sqlclient.URL {
			return &sqlclient.URL{URL: u, DSN: "dsn", Schema: "schema"}
		})),
	)
	require.PanicsWithValue(
		t,
		"sql/sqlclient: Register opener is nil",
		func() { sqlclient.Register("mysql", nil) },
	)
	require.PanicsWithValue(
		t,
		"sql/sqlclient: Register called twice for mysql",
		func() {
			sqlclient.Register("mysql", sqlclient.OpenerFunc(func(context.Context, *url.URL) (*sqlclient.Client, error) {
				return c, nil
			}))
		},
	)
	c1, err := sqlclient.Open(context.Background(), "mysql://:3306")
	require.NoError(t, err)
	require.True(t, c == c1)
	require.Equal(t, "dsn", c.URL.DSN)
	require.Equal(t, "schema", c.URL.Schema)

	c1, err = sqlclient.Open(context.Background(), "maria://:3306")
	require.NoError(t, err)
	require.True(t, c == c1)
	require.Equal(t, "dsn", c.URL.DSN)
	require.Equal(t, "schema", c.URL.Schema)

	c1, err = sqlclient.Open(context.Background(), "postgres://:3306")
	require.EqualError(t, err, `sql/sqlclient: unknown driver "postgres". See: https://atlasgo.io/url`)
}

func TestOpen_Errors(t *testing.T) {
	c, err := sqlclient.Open(context.Background(), "missing")
	require.EqualError(t, err, `sql/sqlclient: missing driver. See: https://atlasgo.io/url`)
	require.Nil(t, c)
	c, err = sqlclient.Open(context.Background(), "unknown://")
	require.EqualError(t, err, `sql/sqlclient: unknown driver "unknown". See: https://atlasgo.io/url`)
	require.Nil(t, c)

	// URLs are not attached to errors.
	_, err = sqlclient.Open(context.Background(), " postgres://user:pass:3306/")
	require.EqualError(t, err, "sql/sqlclient: parse open url: first path segment in URL cannot contain colon")
	_, err = sqlclient.Open(context.Background(), "scheme://hello world")
	require.EqualError(t, err, `sql/sqlclient: parse open url: invalid character " " in host name`)
}

func TestParseURL(t *testing.T) {
	_, err := sqlclient.ParseURL("boring ://")
	require.EqualError(t, err, "first path segment in URL cannot contain colon")
	_, err = sqlclient.ParseURL("\bboring://foo.com:3000")
	require.EqualError(t, err, "net/url: invalid control character in URL")
	_, err = sqlclient.ParseURL("boring:// : @foo.com:3000")
	require.EqualError(t, err, "net/url: invalid userinfo")
}

func TestClient_AddClosers(t *testing.T) {
	var (
		i int
		c = &sqlclient.Client{DB: sql.OpenDB(nil)}
		f = closerFunc(func() error { i++; return nil })
	)
	c.AddClosers(f, f, f)
	require.NoError(t, c.Close())
	require.Equal(t, 3, i)
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

func TestClient_Tx(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	const stmt = "create database `test`"
	mock.ExpectBegin()
	mock.ExpectExec(stmt).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectBegin()
	mock.ExpectExec(stmt).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectRollback()

	var cC, rC bool
	sqlclient.Register(
		"tx",
		sqlclient.OpenerFunc(func(context.Context, *url.URL) (*sqlclient.Client, error) {
			return &sqlclient.Client{Name: "tx", DB: db, Driver: &mockDriver{db: db}}, nil
		}),
		sqlclient.RegisterDriverOpener(func(db schema.ExecQuerier) (migrate.Driver, error) {
			return &mockDriver{db: db}, nil
		}),
		sqlclient.RegisterTxOpener(func(ctx context.Context, db *sql.DB, opts *sql.TxOptions) (*sqlclient.Tx, error) {
			tx, err := db.BeginTx(ctx, opts)
			require.NoError(t, err)
			return &sqlclient.Tx{
				Tx: tx,
				CommitFn: func() error {
					cC = true
					return tx.Commit()
				},
				RollbackFn: func() error {
					rC = true
					return tx.Rollback()
				},
			}, nil
		}),
	)

	c, err := sqlclient.Open(context.Background(), "tx://")
	require.NoError(t, err)

	// Commit works.
	tx, err := c.Tx(context.Background(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(context.Background(), stmt)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.True(t, cC)

	// Rollback works as well.
	tx, err = c.Tx(context.Background(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(context.Background(), stmt)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	require.True(t, rC)

	require.NoError(t, mock.ExpectationsWereMet())
}

type mockDriver struct {
	migrate.Driver
	db schema.ExecQuerier
}

func (m *mockDriver) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return m.db.ExecContext(ctx, query, args...)
}

func TestClientHooks(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	sqlclient.Register(
		"hook",
		sqlclient.OpenerFunc(func(context.Context, *url.URL) (*sqlclient.Client, error) {
			return &sqlclient.Client{Name: "tx", DB: db, Driver: &mockDriver{db: db}}, nil
		}),
		sqlclient.RegisterDriverOpener(func(db schema.ExecQuerier) (migrate.Driver, error) {
			return &mockDriver{db: db}, nil
		}),
	)
	var (
		calls [5]int
		hk    = &sqlclient.Hook{}
	)
	hk.Conn.AfterOpen = func(context.Context, *sqlclient.Client) error {
		calls[0]++
		return nil
	}
	hk.Conn.BeforeClose = func(*sqlclient.Client) error {
		calls[1]++
		return nil
	}
	hk.Tx.AfterBegin = func(context.Context, *sqlclient.TxClient) error {
		calls[2]++
		return nil
	}
	hk.Tx.BeforeCommit = func(*sqlclient.TxClient) error {
		calls[3]++
		return nil
	}
	hk.Tx.BeforeRollback = func(*sqlclient.TxClient) error {
		calls[4]++
		return nil
	}
	mock.ExpectClose()
	oc, err := sqlclient.Open(context.Background(), "hook://", sqlclient.OpenWithHooks(hk))
	require.NoError(t, err)
	require.NoError(t, oc.Close())
	require.Equal(t, [5]int{1, 1, 0, 0, 0}, calls)
	require.NoError(t, mock.ExpectationsWereMet())

	db, mock, err = sqlmock.New()
	require.NoError(t, err)
	mock.ExpectBegin()
	mock.ExpectCommit()
	mock.ExpectClose()
	oc, err = sqlclient.Open(context.Background(), "hook://", sqlclient.OpenWithHooks(hk))
	require.NoError(t, err)
	tc, err := oc.Tx(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, tc.Commit())
	require.NoError(t, oc.Close())
	require.Equal(t, [5]int{2, 2, 1, 1, 0}, calls)
	require.NoError(t, mock.ExpectationsWereMet())

	db, mock, err = sqlmock.New()
	require.NoError(t, err)
	mock.ExpectBegin()
	mock.ExpectRollback()
	mock.ExpectClose()
	oc, err = sqlclient.Open(context.Background(), "hook://", sqlclient.OpenWithHooks(hk))
	require.NoError(t, err)
	tc, err = oc.Tx(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, tc.Rollback())
	require.NoError(t, oc.Close())
	require.Equal(t, [5]int{3, 3, 2, 1, 1}, calls)
	require.NoError(t, mock.ExpectationsWereMet())

	// Open hook failed.
	hk.Conn.AfterOpen = func(context.Context, *sqlclient.Client) error {
		calls[0]++
		return errors.New("open failed")
	}
	db, mock, err = sqlmock.New()
	require.NoError(t, err)
	mock.ExpectClose()
	oc, err = sqlclient.Open(context.Background(), "hook://", sqlclient.OpenWithHooks(hk))
	require.EqualError(t, err, "open failed")
	require.Equal(t, [5]int{4, 3, 2, 1, 1}, calls, "close hooks should not be called")
	require.NoError(t, mock.ExpectationsWereMet())

	// After begin hook failed.
	hk.Conn.AfterOpen = func(context.Context, *sqlclient.Client) error {
		calls[0]++
		return nil
	}
	hk.Tx.AfterBegin = func(context.Context, *sqlclient.TxClient) error {
		calls[2]++
		return errors.New("after begin failed")
	}
	db, mock, err = sqlmock.New()
	require.NoError(t, err)
	mock.ExpectBegin()
	mock.ExpectRollback()
	mock.ExpectClose()
	oc, err = sqlclient.Open(context.Background(), "hook://", sqlclient.OpenWithHooks(hk))
	require.NoError(t, err)
	tc, err = oc.Tx(context.Background(), nil)
	require.EqualError(t, err, "after begin failed")
	require.NoError(t, oc.Close())
	require.Equal(t, [5]int{5, 4, 3, 1, 1}, calls, "rollback hooks should not be called")
}
