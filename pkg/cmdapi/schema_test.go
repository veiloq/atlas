// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package cmdapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/veiloq/atlas/pkg/cmdlog"
	"github.com/veiloq/atlas/sql/migrate"
	"github.com/veiloq/atlas/sql/schema"
	"github.com/veiloq/atlas/sql/sqlclient"
)

const (
	unformatted = `block  "x"  {
 x = 1
    y     = 2
}
`
	formatted = `block "x" {
  x = 1
  y = 2
}
`
)

func TestSchema_Diff(t *testing.T) {
	// Creates the missing table.
	s, err := runCmd(
		schemaDiffCmd(),
		"--from", openSQLite(t, ""),
		"--to", openSQLite(t, "create table t1 (id int);"),
	)
	require.NoError(t, err)
	require.EqualValues(t, "-- Create \"t1\" table\nCREATE TABLE `t1` (`id` int NULL);\n", s)

	// Format indentation one table.
	s, err = runCmd(
		schemaDiffCmd(),
		"--from", openSQLite(t, ""),
		"--to", openSQLite(t, "create table t1 (id int);"),
		"--format", `{{ sql . "  " }}`,
	)
	require.NoError(t, err)
	require.EqualValues(t, "-- Create \"t1\" table\nCREATE TABLE `t1` (\n  `id` int NULL\n);\n", s)

	// No changes.
	s, err = runCmd(
		schemaDiffCmd(),
		"--from", openSQLite(t, ""),
		"--to", openSQLite(t, ""),
	)
	require.NoError(t, err)
	require.EqualValues(t, "Schemas are synced, no changes to be made.\n", s)

	// Format no changes.
	s, err = runCmd(
		schemaDiffCmd(),
		"--from", openSQLite(t, ""),
		"--to", openSQLite(t, ""),
		"--format", `{{ sql . " " }}`,
	)
	require.NoError(t, err)
	require.Empty(t, s)

	// Desired state from migration directory requires dev database.
	_, err = runCmd(
		schemaDiffCmd(),
		"--from", "file://testdata/sqlite",
		"--to", openSQLite(t, ""),
	)
	require.EqualError(t, err, "--dev-url cannot be empty. See: https://atlasgo.io/atlas-schema/sql#dev-database")

	// Desired state from migration directory.
	s, err = runCmd(
		schemaDiffCmd(),
		"--from", openSQLite(t, ""),
		"--to", "file://testdata/sqlite",
		"--dev-url", openSQLite(t, ""),
	)
	require.NoError(t, err)
	require.EqualValues(t, "-- Create \"tbl\" table\nCREATE TABLE `tbl` (`col` int NOT NULL, `col_2` bigint NULL);\n", s)

	// Desired state from migration directory.
	s, err = runCmd(
		schemaDiffCmd(),
		"--from", openSQLite(t, ""),
		"--to", "file://testdata/sqlite",
		"--dev-url", openSQLite(t, ""),
	)
	require.NoError(t, err)
	require.EqualValues(t, "-- Create \"tbl\" table\nCREATE TABLE `tbl` (`col` int NOT NULL, `col_2` bigint NULL);\n", s)

	// Current state from migration directory, desired state from HCL - synced.
	p := filepath.Join(t.TempDir(), "schema.hcl")
	require.NoError(t, os.WriteFile(p, []byte(`schema "main" {}
table "tbl" {
  schema = schema.main
  column "col" {
    type = int
  }
  column "col_2" {
    type = bigint
    null = true
  }
}`), 0644))
	s, err = runCmd(
		schemaDiffCmd(),
		"--from", "file://testdata/sqlite",
		"--to", "file://"+p,
		"--dev-url", openSQLite(t, ""),
	)
	require.NoError(t, err)
	require.EqualValues(t, "Schemas are synced, no changes to be made.\n", s)

	// Current state from migration directory, desired state from HCL - missing column.
	p = filepath.Join(t.TempDir(), "schema.hcl")
	require.NoError(t, os.WriteFile(p, []byte(`schema "main" {}
table "tbl" {
  schema = schema.main
  column "col" {
    type = int
  }
  column "col_2" {
    type = bigint
    null = true
  }
  column "col_3" {
    type = text
  }
}`), 0644))
	s, err = runCmd(
		schemaDiffCmd(),
		"--from", "file://testdata/sqlite",
		"--to", "file://"+p,
		"--dev-url", openSQLite(t, ""),
	)
	require.NoError(t, err)
	require.EqualValues(
		t,
		"-- Add column \"col_3\" to table: \"tbl\"\nALTER TABLE `tbl` ADD COLUMN `col_3` text NOT NULL;\n",
		s,
	)

	// Current state from migration directory with version, desired state from HCL - two missing columns.
	s, err = runCmd(
		schemaDiffCmd(),
		"--from", "file://testdata/sqlite?version=20220318104614",
		"--to", "file://"+p,
		"--dev-url", openSQLite(t, ""),
	)
	require.NoError(t, err)
	require.EqualValues(
		t,
		"-- Add column \"col_2\" to table: \"tbl\"\n"+
			"ALTER TABLE `tbl` ADD COLUMN `col_2` bigint NULL;\n"+
			"-- Add column \"col_3\" to table: \"tbl\"\n"+
			"ALTER TABLE `tbl` ADD COLUMN `col_3` text NOT NULL;\n",
		s,
	)

	// Current state from migration directory, desired state from multi file HCL - missing column.
	p = t.TempDir()
	var (
		one = filepath.Join(p, "one.hcl")
		two = filepath.Join(p, "two.hcl")
	)
	require.NoError(t, os.WriteFile(one, []byte(`table "tbl" {
  schema = schema.main
  column "col" {
    type = int
  }
  column "col_2" {
    type = bigint
    null = true
  }
  column "col_3" {
    type = text
  }
}`), 0644))
	require.NoError(t, os.WriteFile(two, []byte(`schema "main" {}`), 0644))
	s, err = runCmd(
		schemaDiffCmd(),
		"--from", "file://testdata/sqlite",
		"--to", "file://"+p,
		"--dev-url", openSQLite(t, ""),
	)
	require.NoError(t, err)
	require.EqualValues(
		t,
		"-- Add column \"col_3\" to table: \"tbl\"\nALTER TABLE `tbl` ADD COLUMN `col_3` text NOT NULL;\n",
		s,
	)
	s, err = runCmd(
		schemaDiffCmd(),
		"--from", "file://testdata/sqlite",
		"--to", "file://"+one,
		"--to", "file://"+two,
		"--dev-url", openSQLite(t, ""),
	)
	require.NoError(t, err)
	require.EqualValues(
		t,
		"-- Add column \"col_3\" to table: \"tbl\"\nALTER TABLE `tbl` ADD COLUMN `col_3` text NOT NULL;\n",
		s,
	)

	t.Run("FromConfig", func(t *testing.T) {
		var (
			p   = t.TempDir()
			cp  = filepath.Join(p, "atlas.hcl")
			sp  = filepath.Join(p, "schema.hcl")
			cfg = fmt.Sprintf(`
env "local" {
  dev = "%s"
  format {
    schema {
      diff = "{{ sql . \"\t\" }}"
    }
  }
}`, openSQLite(t, ""))
		)
		require.NoError(t, os.WriteFile(cp, []byte(cfg), 0600))
		require.NoError(t, os.WriteFile(sp, []byte(`
schema "main" {}
table "users" {
  schema = schema.main
  column "id" {
    type = int
  }
}
`), 0600))

		cmd := schemaCmd()
		cmd.AddCommand(schemaDiffCmd())
		s, err := runCmd(
			cmd, "diff",
			"-c", "file://"+cp,
			"--env", "local",
			"--to", "file://"+sp,
			"--from", openSQLite(t, ""),
		)
		require.NoError(t, err)
		require.Equal(t, "-- Create \"users\" table\nCREATE TABLE `users` (\n\t`id` int NOT NULL\n);\n", s)
	})

	t.Run("SkipChanges", func(t *testing.T) {
		var (
			p   = t.TempDir()
			cfg = filepath.Join(p, "atlas.hcl")
		)
		err = os.WriteFile(cfg, []byte(`
variable "destructive" {
  type = bool
  default = false
}

env "local" {
  diff {
    skip {
      drop_table = !var.destructive
    }
  }
}
`), 0600)
		require.NoError(t, err)

		// Skip destructive changes.
		cmd := schemaCmd()
		cmd.AddCommand(schemaDiffCmd())
		s, err := runCmd(
			cmd, "diff",
			"-c", "file://"+cfg,
			"--from", openSQLite(t, "create table users (id int);"),
			"--to", openSQLite(t, ""),
			"--env", "local",
		)
		require.NoError(t, err)
		require.Equal(t, "Schemas are synced, no changes to be made.\n", s)

		// Apply destructive changes.
		cmd = schemaCmd()
		cmd.AddCommand(schemaDiffCmd())
		s, err = runCmd(
			cmd, "diff",
			"-c", "file://"+cfg,
			"--from", openSQLite(t, "create table users (id int);"),
			"--to", openSQLite(t, ""),
			"--env", "local",
			"--var", "destructive=true",
		)
		require.NoError(t, err)
		lines := strings.Split(strings.TrimSpace(s), "\n")
		require.Equal(t, []string{
			"-- Disable the enforcement of foreign-keys constraints",
			"PRAGMA foreign_keys = off;",
			`-- Drop "users" table`,
			"DROP TABLE `users`;",
			"-- Enable back the enforcement of foreign-keys constraints",
			"PRAGMA foreign_keys = on;",
		}, lines)
	})

	t.Run("FromConfig/DevURL", func(t *testing.T) {
		var (
			p    = t.TempDir()
			cfg  = filepath.Join(p, "atlas.hcl")
			from = filepath.Join(p, "schema1.sql")
		)
		err = os.WriteFile(cfg, []byte(`
env "local" {
  dev = "sqlite://dev?mode=memory&_fk=1"
  exclude = ["posts"]
}
`), 0600)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(from, []byte(`CREATE TABLE users (id int);`), 0600))
		cmd := schemaCmd()
		cmd.AddCommand(schemaDiffCmd())
		s, err := runCmd(
			cmd, "diff",
			"--from", "file://"+from,
			"--to", openSQLite(t, "CREATE TABLE users (id int, name text); CREATE TABLE posts (id int);"),
			"--config", "file://"+cfg,
			"--env", "local",
		)
		require.NoError(t, err)
		require.Equal(t, "-- Add column \"name\" to table: \"users\"\nALTER TABLE `users` ADD COLUMN `name` text NULL;\n", s)
	})

	t.Run("CompareDataSrc", func(t *testing.T) {
		var (
			p        = t.TempDir()
			cfg      = filepath.Join(p, "atlas.hcl")
			from, to = filepath.Join(p, "schema_from.hcl"), filepath.Join(p, "schema_to.hcl")
		)
		err = os.WriteFile(cfg, []byte(`
variable "from_path" {
  type = string
}
variable "to_path" {
  type = string
}
data "hcl_schema" "from" {
  path = var.from_path
}
data "hcl_schema" "to" {
  path = var.to_path
}
env "drift" {
  dev = "sqlite://dev?mode=memory&_fk=1"
  # Variables defined and available with env:// prefix.
  from_schema = data.hcl_schema.from.url
  to_schema   = data.hcl_schema.to.url
}
`), 0600)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(from, []byte(`
schema "main" {}
table "t1" {
  schema = schema.main
  column "id" {
    type = int
  }
}`), 0600))
		require.NoError(t, os.WriteFile(to, []byte(`
schema "main" {}
table "t2" {
  schema = schema.main
  column "id" {
    type = int
  }
}`), 0600))

		cmd := schemaCmd()
		cmd.AddCommand(schemaDiffCmd())
		s, err := runCmd(
			cmd, "diff",
			"--config", "file://"+cfg,
			"--env", "drift",
			"--var", "from_path="+from,
			"--var", "to_path="+to,
			"--from", "env://from_schema",
			"--to", "env://to_schema",
		)
		require.NoError(t, err)
		require.Equal(t, []string{
			"-- Disable the enforcement of foreign-keys constraints",
			"PRAGMA foreign_keys = off;",
			`-- Drop "t1" table`,
			"DROP TABLE `t1`;",
			`-- Create "t2" table`,
			"CREATE TABLE `t2` (`id` int NOT NULL);",
			"-- Enable back the enforcement of foreign-keys constraints",
			"PRAGMA foreign_keys = on;",
		}, strings.Split(strings.TrimSpace(s), "\n"))
	})
	t.Run("InspectDataSrc", func(t *testing.T) {
		var (
			p   = t.TempDir()
			cfg = filepath.Join(p, "atlas.hcl")
			app = filepath.Join(p, "schema.hcl")
		)
		err = os.WriteFile(cfg, []byte(`
variable "path" {
  type = string
}
data "hcl_schema" "app" {
  path = var.path
}
env "app" {
  dev = "sqlite://dev?mode=memory&_fk=1"
  # Variables defined and available with env:// prefix.
  app = data.hcl_schema.app.url
}
`), 0600)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(app, []byte(`
schema "main" {}
table "t1" {
  schema = schema.main
  column "id" {
    type = int
  }
}`), 0600))

		cmd := schemaCmd()
		cmd.AddCommand(schemaInspectCmd())
		s, err := runCmd(
			cmd, "inspect",
			"--config", "file://"+cfg,
			"--env", "app",
			"--var", "path="+app,
			"--url", "env://app",
		)
		require.NoError(t, err)
		require.Equal(t, `table "t1" {
  schema = schema.main
  column "id" {
    null = false
    type = int
  }
}
schema "main" {
}
`, s)
	})
}

func TestSchema_Apply(t *testing.T) {
	const drvName = "checknormalizer"
	// If no dev-database is given, there must not be a call to Driver.Normalize.
	sqlclient.Register(
		drvName,
		sqlclient.OpenerFunc(func(ctx context.Context, url *url.URL) (*sqlclient.Client, error) {
			url.Scheme = "sqlite"
			c, err := sqlclient.OpenURL(ctx, url)
			if err != nil {
				return nil, err
			}
			c.Driver = &assertNormalizerDriver{t: t, Driver: c.Driver}
			return c, nil
		}),
	)

	p := filepath.Join(t.TempDir(), "schema.hcl")
	require.NoError(t, os.WriteFile(p, []byte(`schema "my_schema" {}`), 0644))
	_, _ = runCmd(
		schemaApplyCmd(),
		"--url", drvName+"://?mode=memory",
		"-f", p,
	)
}

func TestSchema_ApplyLog(t *testing.T) {
	t.Run("DryRun", func(t *testing.T) {
		db := openSQLite(t, "")
		cmd := schemaCmd()
		cmd.AddCommand(schemaApplyCmd())
		s, err := runCmd(
			cmd, "apply",
			"-u", db,
			"--to", openSQLite(t, ""),
			"--dry-run",
			"--format", "{{ json .Changes }}",
		)
		require.NoError(t, err)
		require.Equal(t, "{}", s)

		cmd = schemaCmd()
		cmd.AddCommand(schemaApplyCmd())
		s, err = runCmd(
			cmd, "apply",
			"-u", db,
			"--to", openSQLite(t, "create table t1 (id int);"),
			"--dry-run",
			"--format", "{{ json .Changes }}",
		)
		require.NoError(t, err)
		require.Equal(
			t, "{\"Pending\":[\"CREATE TABLE `t1` (\\n  `id` int NULL\\n)\"]}",
			strings.ReplaceAll(s, ";", ""), // Compatibility between ent/oss.
		)
	})

	t.Run("AutoApprove", func(t *testing.T) {
		db := openSQLite(t, "")
		cmd := schemaCmd()
		cmd.AddCommand(schemaApplyCmd())
		s, err := runCmd(
			cmd, "apply",
			"-u", db,
			"--to", openSQLite(t, "create table t1 (id int);"),
			"--auto-approve",
			"--format", "{{ json .Changes }}",
		)
		require.NoError(t, err)
		require.Equal(
			t, "{\"Applied\":[\"CREATE TABLE `t1` (\\n  `id` int NULL\\n)\"]}",
			strings.ReplaceAll(s, ";", ""), // Compatibility between ent/oss.
		)

		cmd = schemaCmd()
		cmd.AddCommand(schemaApplyCmd())
		s, err = runCmd(
			cmd, "apply",
			"-u", db,
			"--to", openSQLite(t, "create table t1 (id int);"),
			"--auto-approve",
			"--format", "{{ json .Changes }}",
		)
		require.NoError(t, err)
		require.Equal(t, "{}", s)

		cmd = schemaCmd()
		cmd.AddCommand(schemaApplyCmd())
		s, err = runCmd(
			cmd, "apply",
			"-u", db,
			"--to", openSQLite(t, "create table t2 (id int);"),
			"--auto-approve",
			"--format", "{{ json .Changes }}",
		)
		require.NoError(t, err)
		require.Equal(
			t, "{\"Applied\":[\"PRAGMA foreign_keys = off\",\"DROP TABLE `t1`\",\"CREATE TABLE `t2` (\\n  `id` int NULL\\n)\",\"PRAGMA foreign_keys = on\"]}",
			strings.ReplaceAll(s, ";", ""), // Compatibility between ent/oss.
		)

		// Simulate a failed execution.
		conn, err := sql.Open("sqlite3", strings.TrimPrefix(db, "sqlite://"))
		require.NoError(t, err)
		_, err = conn.Exec("INSERT INTO t2 (`id`) VALUES (1), (1)")
		require.NoError(t, err)

		cmd = schemaCmd()
		cmd.AddCommand(schemaApplyCmd())
		s, err = runCmd(
			cmd, "apply",
			"-u", db,
			"--to", openSQLite(t, "create table t2 (id int, c int);create unique index t2_id on t2 (id);"),
			"--auto-approve",
			"--format", "{{ json .Changes }}\n",
		)
		require.ErrorContains(t, err, `UNIQUE constraint failed: t2.id`)
		var out struct {
			Applied []string
			Pending []string
			Error   cmdlog.StmtError
		}
		require.NoError(t, json.NewDecoder(strings.NewReader(s)).Decode(&out))
		require.Len(t, out.Applied, 1)
		require.Len(t, out.Pending, 1)
		require.Equal(t, "ALTER TABLE `t2` ADD COLUMN `c` int NULL", strings.TrimRight(out.Applied[0], ";"))
		require.Equal(t, "CREATE UNIQUE INDEX `t2_id` ON `t2` (`id`)", strings.TrimRight(out.Pending[0], ";"))
		require.Equal(t, out.Pending[0], out.Error.Stmt)
		require.Contains(t, out.Error.Text, `UNIQUE constraint failed: t2.id`)
	})
}

func TestSchema_ApplySchemaMismatch(t *testing.T) {
	var (
		p   = t.TempDir()
		src = filepath.Join(p, "schema.hcl")
	)
	// SQLite always has a schema called "main".
	err := os.WriteFile(src, []byte(`
schema "hello" {}
`), 0600)
	require.NoError(t, err)
	cmd := schemaCmd()
	cmd.AddCommand(schemaApplyCmd())
	_, err = runCmd(
		cmd, "apply",
		"-u", openSQLite(t, ""),
		"-f", src,
	)
	require.EqualError(t, err, `mismatched HCL and database schemas: "main" <> "hello"`)
}

func TestSchema_ApplySkip(t *testing.T) {
	var (
		p   = t.TempDir()
		cfg = filepath.Join(p, "atlas.hcl")
		src = filepath.Join(p, "schema.hcl")
	)
	err := os.WriteFile(src, []byte(`
schema "main" {}

table "users" {
  schema = schema.main
  column "id" {
    type = int
  }
}
`), 0600)
	require.NoError(t, err)
	err = os.WriteFile(cfg, []byte(`
variable "schema" {
  type = string
  default = "dev"
}

variable "destructive" {
  type = bool
  default = false
}

env "local" {
  src = var.schema
  dev_url = "sqlite://dev?mode=memory&_fk=1"
}

diff {
  skip {
    drop_table = !var.destructive
  }
}
`), 0600)
	require.NoError(t, err)

	// Skip destructive changes.
	cmd := schemaCmd()
	cmd.AddCommand(schemaApplyCmd())
	s, err := runCmd(
		cmd, "apply",
		"-u", openSQLite(t, "create table pets (id int);"),
		"-c", "file://"+cfg,
		"--var", "schema=file://"+src,
		"--env", "local",
		"--auto-approve",
		"--format", "{{ json .Changes }}",
	)
	require.NoError(t, err)
	require.Equal(
		t, "{\"Applied\":[\"CREATE TABLE `users` (\\n  `id` int NOT NULL\\n)\"]}",
		strings.ReplaceAll(s, ";", ""), // Compatibility between ent/oss.
	)

	// Skip destructive changes by using project-level policy (no --env was passed).
	cmd = schemaCmd()
	cmd.AddCommand(schemaApplyCmd())
	s, err = runCmd(
		cmd, "apply",
		"-u", openSQLite(t, "create table pets (id int);"),
		"-c", "file://"+cfg, // Using the project-level policy.
		"--to", "file://"+src,
		"--dev-url", "sqlite://dev?mode=memory&_fk=1",
		"--auto-approve",
		"--format", "{{ json .Changes }}",
	)
	require.NoError(t, err)
	require.Equal(
		t, "{\"Applied\":[\"CREATE TABLE `users` (\\n  `id` int NOT NULL\\n)\"]}",
		strings.ReplaceAll(s, ";", ""), // Compatibility between ent/oss.
	)

	// Apply destructive changes.
	cmd = schemaCmd()
	cmd.AddCommand(schemaApplyCmd())
	s, err = runCmd(
		cmd, "apply",
		"-u", openSQLite(t, "create table pets (id int);"),
		"-c", "file://"+cfg,
		"--var", "schema=file://"+src,
		"--var", "destructive=true",
		"--env", "local",
		"--auto-approve",
		"--format", "{{ json .Changes }}",
	)
	require.NoError(t, err)
	require.Equal(
		t, "{\"Applied\":[\"PRAGMA foreign_keys = off\",\"DROP TABLE `pets`\",\"CREATE TABLE `users` (\\n  `id` int NOT NULL\\n)\",\"PRAGMA foreign_keys = on\"]}",
		strings.ReplaceAll(s, ";", ""), // Compatibility between ent/oss.
	)
}

func TestSchema_ApplySources(t *testing.T) {
	var (
		p   = t.TempDir()
		cfg = filepath.Join(p, "atlas.hcl")
		src = []string{filepath.Join(p, "one.hcl"), filepath.Join(p, "two.hcl")}
	)
	err := os.WriteFile(src[0], []byte(`
schema "main" {}

table "one" {
  schema = schema.main
  column "id" {
    type = int
  }
}
`), 0600)
	require.NoError(t, err)
	err = os.WriteFile(src[1], []byte(`
table "two" {
  schema = schema.main
  column "id" {
    type = int
  }
}
`), 0600)
	require.NoError(t, err)
	err = os.WriteFile(cfg, []byte(fmt.Sprintf(`
env "local" {
  src = [%q, %q]
}`, src[0], src[1])), 0600)
	require.NoError(t, err)

	cmd := schemaCmd()
	cmd.AddCommand(schemaApplyCmd())
	s, err := runCmd(
		cmd, "apply",
		"-u", openSQLite(t, ""),
		"-c", "file://"+cfg,
		"--env", "local",
		"--auto-approve",
		"--format", "{{ json .Changes }}",
	)
	require.NoError(t, err)
	require.Equal(
		t, "{\"Applied\":[\"CREATE TABLE `one` (\\n  `id` int NOT NULL\\n)\",\"CREATE TABLE `two` (\\n  `id` int NOT NULL\\n)\"]}",
		strings.ReplaceAll(s, ";", ""), // Compatibility between ent/oss.
	)
}

func TestSchema_ToFlagPrecedence(t *testing.T) {
	var (
		p       = t.TempDir()
		cfg     = filepath.Join(p, "atlas.hcl")
		hclFile = filepath.Join(p, "schema.hcl")
		sqlFile = filepath.Join(p, "schema.sql")
	)
	err := os.WriteFile(hclFile, []byte(`
schema "main" {}

table "one" {
  schema = schema.main
  column "id" {
    type = int
  }
}
`), 0600)
	require.NoError(t, err)
	err = os.WriteFile(sqlFile, []byte("create table tbl (col integer)"), 0600)
	require.NoError(t, err)
	err = os.WriteFile(cfg, []byte(fmt.Sprintf(`
env "local" {
  src = "file://%s"
}`, sqlFile)), 0600)
	require.NoError(t, err)

	cmd := schemaCmd()
	cmd.AddCommand(schemaApplyCmd())
	s, err := runCmd(
		cmd, "apply",
		"-u", openSQLite(t, ""),
		"-c", "file://"+cfg,
		"--to", "file://"+hclFile,
		"--env", "local",
		"--auto-approve",
		"--format", "{{ json .Changes }}",
	)
	require.NoError(t, err)
	require.Equal(
		t, "{\"Applied\":[\"CREATE TABLE `one` (\\n  `id` int NOT NULL\\n)\"]}",
		strings.ReplaceAll(s, ";", ""), // Compatibility between ent/oss.
	)
}

func TestSchema_ApplySQL(t *testing.T) {
	t.Run("File", func(t *testing.T) {
		db := openSQLite(t, "")
		p := filepath.Join(t.TempDir(), "schema.sql")
		require.NoError(t, os.WriteFile(p, []byte(`create table t1 (id int NOT NULL);`), 0600))
		s, err := runCmd(
			schemaApplyCmd(),
			"-u", db,
			"--dev-url", openSQLite(t, ""),
			"--to", "file://"+p,
			"--auto-approve",
			"--format", "{{ json .Changes }}",
		)
		require.NoError(t, err)
		require.Equal(
			t, "{\"Applied\":[\"CREATE TABLE `t1` (\\n  `id` int NOT NULL\\n)\"]}",
			strings.ReplaceAll(s, ";", ""), // Compatibility between ent/oss.
		)

		s, err = runCmd(
			schemaApplyCmd(),
			"-u", db,
			"--dev-url", openSQLite(t, ""),
			"--to", "file://"+p,
			"--auto-approve",
		)
		require.NoError(t, err)
		require.Equal(t, "Schema is synced, no changes to be made\n", s)
	})
	t.Run("Dir", func(t *testing.T) {
		db := openSQLite(t, "")
		s, err := runCmd(
			schemaApplyCmd(),
			"-u", db,
			"--dev-url", openSQLite(t, ""),
			"--to", "file://testdata/sqlite",
			"--auto-approve",
			"--format", "{{ json .Changes }}",
		)
		require.NoError(t, err)
		require.Equal(
			t, "{\"Applied\":[\"CREATE TABLE `tbl` (\\n  `col` int NOT NULL,\\n  `col_2` bigint NULL\\n)\"]}",
			strings.ReplaceAll(s, ";", ""), // Compatibility between ent/oss.
		)

		s, err = runCmd(
			schemaApplyCmd(),
			"-u", db,
			"--dev-url", openSQLite(t, ""),
			"--to", "file://testdata/sqlite",
			"--auto-approve",
		)
		require.NoError(t, err)
		require.Equal(t, "Schema is synced, no changes to be made\n", s)
	})
	t.Run("Error", func(t *testing.T) {
		_, err := runCmd(
			schemaApplyCmd(),
			"-u", openSQLite(t, ""),
			"--dev-url", openSQLite(t, ""),
			"--to", "file://testdata/sqlite",
			"--to", "file://testdata/sqlite2",
		)
		require.Error(t, err)

		_, err = runCmd(
			schemaApplyCmd(),
			"-u", openSQLite(t, ""),
			"--dev-url", openSQLite(t, ""),
			"--to", "file://"+t.TempDir(),
		)
		require.ErrorContains(t, err, `contains neither SQL nor HCL files`)

		p := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(p, "schema.sql"), []byte(`create table t1 (id int NOT NULL);`), 0600))
		require.NoError(t, os.WriteFile(filepath.Join(p, "schema.hcl"), []byte(`schema "main" {}`), 0600))
		_, err = runCmd(
			schemaApplyCmd(),
			"-u", openSQLite(t, ""),
			"--dev-url", openSQLite(t, ""),
			"--to", "file://"+p,
		)
		require.EqualError(t, err, `ambiguous schema: both SQL and HCL files found: "schema.hcl", "schema.sql"`)

		_, err = runCmd(
			schemaApplyCmd(),
			"-u", openSQLite(t, ""),
			"--dev-url", openSQLite(t, ""),
			"--to", "testdata/sqlite",
		)
		require.EqualError(t, err, "missing scheme. Did you mean file://testdata/sqlite?")
	})
	t.Run("TxMode", func(t *testing.T) {
		db := openSQLite(t, "")
		p := filepath.Join(t.TempDir(), "schema.hcl")
		require.NoError(t, os.WriteFile(p, []byte(`schema "main" {}

table "ok" {
  schema = schema.main
  column "id" {
    type = int
  }
}

table "bad" {
  schema = schema.main
  column "id" {
    type = int
    default = "invalid check"
  }
}`), 0600))
		s, err := runCmd(
			schemaApplyCmd(),
			"-u", db,
			"--to", "file://"+p,
			"--auto-approve",
			"--format", "{{ json .Changes }}",
		)
		require.Error(t, err)
		require.Contains(
			t, strings.ReplaceAll(s, ";", ""), // Compatibility between ent/oss.
			"{\"Applied\":[\"CREATE TABLE `ok` (\\n  `id` int NOT NULL\\n)\"],\"Pending\":[\"CREATE TABLE `bad` (\\n  `id` int NOT NULL DEFAULT invalid check\\n)\"],\"Error\":{\"Stmt\":\"CREATE TABLE `bad` (\\n  `id` int NOT NULL DEFAULT invalid check\\n)\",",
		)
		require.Regexp(t, `Error: .* near "\)": syntax error`, s)

		// The "ok" table should be created, as changes are rolled back on error.
		s, err = runCmd(schemaInspectCmd(), "-u", db)
		require.NoError(t, err)
		require.Equal(t, "schema \"main\" {\n}\n", s)
	})
}

func TestSchema_ApplyReview(t *testing.T) {
	t.Run("mutex-auto-approve", func(t *testing.T) {
		cfg := filepath.Join(t.TempDir(), "atlas.hcl")
		require.NoError(t, os.WriteFile(cfg, []byte(`env "test" {
  lint {
    review = WARNING
  }
}`), 0600))
		db := openSQLite(t, "")
		p := filepath.Join(t.TempDir(), "schema.sql")
		require.NoError(t, os.WriteFile(p, []byte(`create table t1 (id int NOT NULL);`), 0600))
		cmd := schemaCmd()
		cmd.AddCommand(schemaApplyCmd())
		_, err := runCmd(
			cmd,
			"apply",
			"-c", "file://"+cfg,
			"--url", db,
			"--env", "test",
			"--dev-url", openSQLite(t, ""),
			"--to", "file://"+p,
			"--auto-approve",
		)
		require.ErrorContains(t, err, `auto-approve is not allowed when a lint policy is set to "WARNING"`)
	})
}

func TestSchema_InspectLog(t *testing.T) {
	db := openSQLite(t, "create table t1 (id integer primary key);create table t2 (name text);")
	cmd := schemaCmd()
	cmd.AddCommand(schemaInspectCmd())
	s, err := runCmd(
		cmd, "inspect",
		"-u", db,
		"--format", "{{ json . }}",
	)
	require.NoError(t, err)
	require.Equal(t, `{"schemas":[{"name":"main","tables":[{"name":"t1","columns":[{"name":"id","type":"INTEGER","null":true}],"primary_key":{"parts":[{"column":"id"}]}},{"name":"t2","columns":[{"name":"name","type":"TEXT","null":true}]}]}]}`, s)
}

func TestSchema_InspectFile(t *testing.T) {
	var (
		p   = t.TempDir()
		cp  = filepath.Join(p, "atlas.hcl")
		sp  = filepath.Join(p, "schema.hcl")
		cfg = fmt.Sprintf(`
env "db" {
  url = "%s"
  dev = "docker://should-not-be-opened"
}

env "file" {
  dev = "%s"
}
`, openSQLite(t, "create table t1(c int)"), openSQLite(t, ""))
	)

	require.NoError(t, os.WriteFile(cp, []byte(cfg), 0600))
	require.NoError(t, os.WriteFile(sp, []byte(`
schema "main" {}
table "users" {
  schema = schema.main
  column "id" {
    type = int
  }
}
`), 0600))
	cmd := schemaCmd()
	cmd.AddCommand(schemaInspectCmd())
	s, err := runCmd(
		cmd, "inspect",
		"-c", "file://"+cp,
		"--env", "db",
		"--format", "{{ sql . }}",
	)
	require.NoError(t, err)
	require.Equal(t, "-- Create \"t1\" table\nCREATE TABLE `t1` (`c` int NULL);\n", s)

	cmd = schemaCmd()
	cmd.AddCommand(schemaInspectCmd())
	s, err = runCmd(
		cmd, "inspect",
		"-c", "file://"+cp,
		"--env", "file",
		"--url", "file://"+sp,
		"--format", "{{ sql . }}",
	)
	require.NoError(t, err)
	require.Equal(t, "-- Create \"users\" table\nCREATE TABLE `users` (`id` int NOT NULL);\n", s)
}

func TestFmt(t *testing.T) {
	for _, tt := range []struct {
		name          string
		inputDir      map[string]string
		expectedDir   map[string]string
		expectedFile  string
		expectedOut   string
		args          []string
		expectedPrint bool
	}{
		{
			name: "specific file",
			inputDir: map[string]string{
				"test.hcl": unformatted,
			},
			expectedDir: map[string]string{
				"test.hcl": formatted,
			},
			args:        []string{"test.hcl"},
			expectedOut: "test.hcl\n",
		},
		{
			name: "current dir",
			inputDir: map[string]string{
				"test.hcl": unformatted,
			},
			expectedDir: map[string]string{
				"test.hcl": formatted,
			},
			expectedOut: "test.hcl\n",
		},
		{
			name: "multi path implicit",
			inputDir: map[string]string{
				"test.hcl":  unformatted,
				"test2.hcl": unformatted,
			},
			expectedDir: map[string]string{
				"test.hcl":  formatted,
				"test2.hcl": formatted,
			},
			expectedOut: "test.hcl\ntest2.hcl\n",
		},
		{
			name: "multi path explicit",
			inputDir: map[string]string{
				"test.hcl":  unformatted,
				"test2.hcl": unformatted,
			},
			expectedDir: map[string]string{
				"test.hcl":  formatted,
				"test2.hcl": formatted,
			},
			args:        []string{"test.hcl", "test2.hcl"},
			expectedOut: "test.hcl\ntest2.hcl\n",
		},
		{
			name: "formatted",
			inputDir: map[string]string{
				"test.hcl": formatted,
			},
			expectedDir: map[string]string{
				"test.hcl": formatted,
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := setupFmtTest(t, tt.inputDir)
			out, err := runCmd(schemaFmtCmd(), tt.args...)
			require.NoError(t, err)
			assertDir(t, dir, tt.expectedDir)
			require.EqualValues(t, tt.expectedOut, out)
		})
	}
}

func TestSchema_Clean(t *testing.T) {
	var (
		u      = fmt.Sprintf("sqlite://file:%s?cache=shared&_fk=1", filepath.Join(t.TempDir(), "test.db"))
		c, err = sqlclient.Open(context.Background(), u)
	)
	require.NoError(t, err)

	// Apply migrations onto database.
	_, err = runCmd(migrateApplyCmd(), "--dir", "file://testdata/sqlite", "--url", u)
	require.NoError(t, err)

	// Run clean and expect to be clean.
	_, err = runCmd(migrateApplyCmd(), "--dir", "file://testdata/sqlite", "--url", u)
	require.NoError(t, err)
	s, err := runCmd(schemaCleanCmd(), "--url", u, "--auto-approve")
	require.NoError(t, err)
	require.NotZero(t, s)
	require.NoError(t, c.Driver.CheckClean(context.Background(), nil))
}

func assertDir(t *testing.T, dir string, expected map[string]string) {
	act := make(map[string]string)
	files, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(dir, f.Name()))
		require.NoError(t, err)
		act[f.Name()] = string(contents)
	}
	require.EqualValues(t, expected, act)
}

func setupFmtTest(t *testing.T, inputDir map[string]string) string {
	wd, err := os.Getwd()
	require.NoError(t, err)
	dir, err := os.MkdirTemp(os.TempDir(), "fmt-test-")
	require.NoError(t, err)
	err = os.Chdir(dir)
	require.NoError(t, err)
	t.Cleanup(func() {
		os.RemoveAll(dir)
		os.Chdir(wd) //nolint:errcheck
	})
	for name, contents := range inputDir {
		file := path.Join(dir, name)
		err = os.WriteFile(file, []byte(contents), 0600)
	}
	require.NoError(t, err)
	return dir
}

type assertNormalizerDriver struct {
	migrate.Driver
	t *testing.T
}

// NormalizeSchema returns the normal representation of a schema.
func (d *assertNormalizerDriver) NormalizeSchema(context.Context, *schema.Schema) (*schema.Schema, error) {
	d.t.Fatal("did not expect a call to NormalizeSchema")
	return nil, nil
}

// NormalizeRealm returns the normal representation of a database.
func (d *assertNormalizerDriver) NormalizeRealm(context.Context, *schema.Realm) (*schema.Realm, error) {
	d.t.Fatal("did not expect a call to NormalizeRealm")
	return nil, nil
}
