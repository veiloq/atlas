// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package migrate

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/veiloq/atlas/pkg/migrate/ent/revision"
	"github.com/veiloq/atlas/sql/migrate"
	"github.com/veiloq/atlas/sql/sqlclient"
	"github.com/veiloq/atlas/sql/sqltool"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

func TestFormatter(t *testing.T) {
	u, err := url.Parse("file://migrations")
	require.NoError(t, err)
	f, err := Formatter(u)
	require.NoError(t, err)
	require.Equal(t, migrate.DefaultFormatter, f)

	u, err = url.Parse("file://migrations?format=atlas")
	require.NoError(t, err)
	f, err = Formatter(u)

	u, err = url.Parse("file://migrations?format=flyway")
	require.NoError(t, err)
	f, err = Formatter(u)
	require.NoError(t, err)
	require.Equal(t, sqltool.FlywayFormatter, f)
}

func TestRevisionsForClient(t *testing.T) {
	ctx := context.Background()
	c, err := sqlclient.Open(ctx, "sqlite://?mode=memory")
	require.NoError(t, err)
	var rrw RevisionReadWriter

	rrw, err = RevisionsForClient(ctx, c, "")
	require.NoError(t, err)
	require.NotNil(t, rrw)
	_, ok := rrw.(*EntRevisions)
	require.True(t, ok, "RevisionsForClient should return an EntRevisions")

	drvMock := &mockDriver{Driver: c.Driver, rrw: &migrate.NopRevisionReadWriter{}}
	c.Driver = drvMock
	rrw, err = RevisionsForClient(ctx, c, "")
	require.ErrorContains(t, err, "unexpected revision read-writer type: *migrate.NopRevisionReadWriter")

	drvMock.rrw = &mockrrw{RevisionReadWriter: &migrate.NopRevisionReadWriter{}}
	rrw, err = RevisionsForClient(ctx, c, "")
	require.NoError(t, err)
	_, ok = rrw.(*mockrrw)
	require.True(t, ok, "RevisionsForClient should return a mockrrw")
}

func TestNewEntRevisions(t *testing.T) {
	ctx := context.Background()
	c, err := sqlclient.Open(ctx, "sqlite://?mode=memory")
	require.NoError(t, err)
	r, err := NewEntRevisions(ctx, c)
	require.NoError(t, err)
	runRevisionsTests(ctx, t, c.Driver, r)
}

func TestDirURL(t *testing.T) {
	localDir := t.TempDir()
	tests := []struct {
		name        string
		url         string
		create      bool
		expected    func() migrate.Dir
		expectedErr error
	}{
		{
			name:   "Valid file URL",
			url:    "file://" + localDir,
			create: false,
			expected: func() migrate.Dir {
				d, err := migrate.NewLocalDir(localDir)
				require.NoError(t, err)
				return d
			},
		},
		{
			name:   "Create local dir",
			url:    "file://" + filepath.Join(localDir, "new/dir"),
			create: true,
			expected: func() migrate.Dir {
				d, err := migrate.NewLocalDir(filepath.Join(localDir, "new/dir"))
				require.NoError(t, err)
				return d
			},
		},
		{
			name:        "Dont create local dir",
			url:         "file://" + filepath.Join(localDir, "new/dir/2"),
			create:      false,
			expectedErr: fmt.Errorf("sql/migrate: stat %s: no such file or directory", filepath.Join(localDir, "new/dir/2")),
		},
		{
			name:        "No scheme",
			url:         localDir,
			create:      false,
			expectedErr: fmt.Errorf("missing scheme for dir url. Did you mean %q? ", "file://"+localDir),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, err := Dir(context.Background(), tt.url, tt.create)
			if tt.expectedErr != nil {
				require.EqualError(t, err, tt.expectedErr.Error())
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.expected(), dir)
			}
		})
	}
}

func runRevisionsTests(ctx context.Context, t *testing.T, drv migrate.Driver, r RevisionReadWriter) {
	_, err := drv.ExecContext(ctx, "CREATE VIEW v1(c1) AS SELECT 1;")
	require.NoError(t, err)
	require.NoError(t, r.Migrate(ctx))

	s, err := drv.InspectSchema(ctx, "", nil)
	require.NoError(t, err)
	require.Len(t, s.Tables, 1)
	_, ok := s.Table(revision.Table)
	require.True(t, ok)

	cur, err := r.CurrentRevision(ctx)
	require.True(t, errors.Is(err, migrate.ErrRevisionNotExist))
	require.Nil(t, cur)

	err = r.WriteRevision(ctx, &migrate.Revision{
		Version:         "1",
		Description:     "desc",
		Type:            migrate.RevisionTypeResolved,
		ExecutedAt:      time.Now(),
		Hash:            "hash",
		OperatorVersion: "0.1.0",
	})
	require.NoError(t, err)
	cur, err = r.CurrentRevision(ctx)
	require.NoError(t, err)
	require.Equal(t, "1", cur.Version)

	next := *cur
	next.Version = "2"
	require.NoError(t, r.WriteRevision(ctx, &next))
	cur, err = r.CurrentRevision(ctx)
	require.NoError(t, err)
	require.Equal(t, "2", cur.Version)

	revs, err := r.ReadRevisions(ctx)
	require.NoError(t, err)
	require.Len(t, revs, 2)
	require.Equal(t, "1", revs[0].Version)
	require.Equal(t, "2", revs[1].Version)

	id, err := r.ID(ctx, "v0.10.1")
	require.NoError(t, err)
	require.NotEmpty(t, id)
	id1, err := r.ID(ctx, "v0.10.1")
	require.NoError(t, err)
	require.Equal(t, id, id1, "identifiers should be allocated only once")

	// Identifier is not returned as a revision.
	revs, err = r.ReadRevisions(ctx)
	require.NoError(t, err)
	require.Len(t, revs, 2, "identifiers should not be returned as revisions")
	_, err = r.ReadRevision(ctx, revisionID)
	require.Error(t, err)
	err = r.DeleteRevision(ctx, revisionID)
	require.Error(t, err)
	err = r.WriteRevision(ctx, &migrate.Revision{Version: revisionID})
	require.Error(t, err)

	cur, err = r.CurrentRevision(ctx)
	require.NoError(t, err)
	require.Equal(t, "2", cur.Version)
	require.NoError(t, r.DeleteRevision(ctx, "2"))
	cur, err = r.CurrentRevision(ctx)
	require.NoError(t, err)
	require.Equal(t, "1", cur.Version)
	require.NoError(t, r.DeleteRevision(ctx, "1"))
	cur, err = r.CurrentRevision(ctx)
	require.True(t, errors.Is(err, migrate.ErrRevisionNotExist))
	require.Nil(t, cur)
	revs, err = r.ReadRevisions(ctx)
	require.NoError(t, err)
	require.Len(t, revs, 0)
	id1, err = r.ID(ctx, "v0.10.1")
	require.NoError(t, err)
	require.Equal(t, id, id1)
}

type (
	mockDriver struct {
		migrate.Driver
		rrw migrate.RevisionReadWriter
	}
	mockrrw struct {
		migrate.RevisionReadWriter
	}
)

func (m *mockDriver) RevisionsReadWriter(context.Context, string) (migrate.RevisionReadWriter, error) {
	return m.rrw, nil
}

func (*mockrrw) CurrentRevision(context.Context) (*migrate.Revision, error) { return nil, nil }
func (*mockrrw) Migrate(context.Context) error                              { return nil }
func (*mockrrw) ID(context.Context, string) (string, error)                 { return "", nil }
