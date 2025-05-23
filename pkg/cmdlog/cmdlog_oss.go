// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

//go:build !ent

package cmdlog

import (
	"context"

	"github.com/veiloq/atlas/sql/migrate"
)

// SchemaApply contains a summary of a 'schema apply' execution on a database.
type SchemaApply struct {
	ctx context.Context `json:"-"`
	Env
	Changes Changes `json:"Changes,omitempty"`
	// General error that occurred during execution.
	// e.g., when committing or rolling back a transaction.
	Error string `json:"Error,omitempty"`
}

// NewSchemaApply returns a SchemaApply.
func NewSchemaApply(ctx context.Context, env Env, applied, pending []*migrate.Change, err *StmtError) *SchemaApply {
	return &SchemaApply{
		ctx: ctx,
		Env: env,
		Changes: Changes{
			Applied: applied,
			Pending: pending,
			Error:   err,
		},
	}
}

// NewSchemaPlan returns a SchemaApply only with pending changes.
func NewSchemaPlan(ctx context.Context, env Env, pending []*migrate.Change, err *StmtError) *SchemaApply {
	return NewSchemaApply(ctx, env, nil, pending, err)
}

func (*MigrateApply) MaskedText(s *migrate.Stmt) string {
	return s.Text // Unsupported feature.
}

// MaskedErrorText returns the masked versioned of the error, if caused by a statement.
func (*MigrateApply) MaskedErrorText(e migrate.LogError) string {
	return e.Error.Error() // Unsupported feature.
}
