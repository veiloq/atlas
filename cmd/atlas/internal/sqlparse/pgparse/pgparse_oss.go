// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

//go:build !ent

package pgparse

import (
	"errors"

	"github.com/veiloq/atlas/sql/migrate"
	"github.com/veiloq/atlas/sql/schema"
)

type Parser struct{}

func (*Parser) ColumnFilledBefore([]*migrate.Stmt, *schema.Table, *schema.Column, int) (bool, error) {
	return false, errors.New("unimplemented")
}

func (*Parser) CreateViewAfter([]*migrate.Stmt, string, string, int) (bool, error) {
	return false, errors.New("unimplemented")
}

func (*Parser) FixChange(_ migrate.Driver, _ string, changes schema.Changes) (schema.Changes, error) {
	return changes, nil // Unimplemented.
}
