// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package sqlparse

import (
	"sync"

	"github.com/veiloq/atlas/pkg/sqlparse/myparse"
	"github.com/veiloq/atlas/pkg/sqlparse/pgparse"
	"github.com/veiloq/atlas/pkg/sqlparse/sqliteparse"
	"github.com/veiloq/atlas/sql/migrate"
	"github.com/veiloq/atlas/sql/mysql"
	"github.com/veiloq/atlas/sql/postgres"
	"github.com/veiloq/atlas/sql/schema"
	"github.com/veiloq/atlas/sql/sqlite"
)

// A Parser represents an SQL file parser used to fix, search and enrich schema.Changes.
type Parser interface {
	// FixChange fixes the changes according to the given statement.
	FixChange(d migrate.Driver, stmt string, changes schema.Changes) (schema.Changes, error)

	// ColumnFilledBefore checks if the column was filled with values before the given position
	// in the file. For example:
	//
	//	UPDATE <table> SET <column> = <value>
	//	UPDATE <table> SET <column> = <value> WHERE <column> IS NULL
	//
	ColumnFilledBefore([]*migrate.Stmt, *schema.Table, *schema.Column, int) (bool, error)

	// CreateViewAfter checks if a view was created after the position with the given name
	// to a table. For example:
	//
	//	ALTER TABLE `users` RENAME TO `Users`
	//	CREATE VIEW `users` AS SELECT * FROM `Users`
	//
	CreateViewAfter(stmts []*migrate.Stmt, old, new string, pos int) (bool, error)
}

// drivers specific fixers.
var drivers sync.Map

// Register a fixer with the given name.
func Register(name string, f Parser) {
	drivers.Store(name, f)
}

// ParserFor returns a ChangesFixer for the given driver.
func ParserFor(name string) Parser {
	f, ok := drivers.Load(name)
	if ok {
		return f.(Parser)
	}
	return nil
}

func init() {
	Register(mysql.DriverName, &myparse.FileParser{})
	Register(postgres.DriverName, &pgparse.Parser{})
	Register(sqlite.DriverName, &sqliteparse.FileParser{})
}
