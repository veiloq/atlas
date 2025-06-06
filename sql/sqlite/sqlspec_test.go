// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package sqlite

import (
	"fmt"
	"testing"

	"github.com/veiloq/atlas/sql/internal/spectest"
	"github.com/veiloq/atlas/sql/schema"
	"github.com/stretchr/testify/require"
)

func TestSQLSpec(t *testing.T) {
	f := `
schema "schema" {
}

table "table" {
	schema = schema.schema
	column "id" {
		type = integer
		auto_increment = true
	}
	column "age" {
		type = integer
	}
	column "price" {
		type = integer
	}
	column "account_name" {
		type = varchar(32)
	}
	primary_key {
		columns = [table.table.column.id]
	}
	index "index" {
		unique = true
		columns = [
			table.table.column.id,
			table.table.column.age,
		]
		where = "age <> 0"
	}
	foreign_key "accounts" {
		columns = [
			table.table.column.account_name,
		]
		ref_columns = [
			table.accounts.column.name,
		]
		on_delete = SET_NULL
	}
	check "positive price" {
		expr = "price > 0"
	}
	without_rowid = true
	strict        = false
}

table "accounts" {
	schema = schema.schema
	column "name" {
		type = varchar(32)
	}
	primary_key {
		columns = [table.accounts.column.name]
	}
	strict = true
}
`
	exp := schema.NewRealm(schema.New("schema")).Schemas[0]
	exp.Tables = []*schema.Table{
		{
			Name:   "table",
			Schema: exp,
			Columns: []*schema.Column{
				{
					Name: "id",
					Type: &schema.ColumnType{
						Type: &schema.IntegerType{
							T: "integer",
						},
					},
					Attrs: []schema.Attr{
						&AutoIncrement{},
					},
				},
				{
					Name: "age",
					Type: &schema.ColumnType{
						Type: &schema.IntegerType{
							T: "integer",
						},
					},
				},
				{
					Name: "price",
					Type: &schema.ColumnType{
						Type: &schema.IntegerType{
							T: "integer",
						},
					},
				},
				{
					Name: "account_name",
					Type: &schema.ColumnType{
						Type: &schema.StringType{
							T:    "varchar",
							Size: 32,
						},
					},
				},
			},
			Attrs: []schema.Attr{
				&schema.Check{
					Name: "positive price",
					Expr: "price > 0",
				},
				&WithoutRowID{},
			},
		},
		{
			Name:   "accounts",
			Schema: exp,
			Columns: []*schema.Column{
				{
					Name: "name",
					Type: &schema.ColumnType{
						Type: &schema.StringType{
							T:    "varchar",
							Size: 32,
						},
					},
				},
			},
			Attrs: []schema.Attr{
				&Strict{},
			},
		},
	}
	exp.Tables[0].PrimaryKey = &schema.Index{
		Table: exp.Tables[0],
		Parts: []*schema.IndexPart{
			{SeqNo: 0, C: exp.Tables[0].Columns[0]},
		},
	}
	exp.Tables[0].Indexes = []*schema.Index{
		{
			Name:   "index",
			Table:  exp.Tables[0],
			Unique: true,
			Parts: []*schema.IndexPart{
				{SeqNo: 0, C: exp.Tables[0].Columns[0]},
				{SeqNo: 1, C: exp.Tables[0].Columns[1]},
			},
			Attrs: []schema.Attr{
				&IndexPredicate{P: "age <> 0"},
			},
		},
	}
	exp.Tables[0].ForeignKeys = []*schema.ForeignKey{
		{
			Symbol:     "accounts",
			Table:      exp.Tables[0],
			Columns:    []*schema.Column{exp.Tables[0].Columns[3]},
			RefTable:   exp.Tables[1],
			RefColumns: []*schema.Column{exp.Tables[1].Columns[0]},
			OnDelete:   schema.SetNull,
		},
	}
	exp.Tables[1].PrimaryKey = &schema.Index{
		Table: exp.Tables[1],
		Parts: []*schema.IndexPart{
			{SeqNo: 0, C: exp.Tables[1].Columns[0]},
		},
	}
	exp.Tables[0].Columns[0].AddIndexes(exp.Tables[0].PrimaryKey)
	exp.Tables[0].Columns[0].AddIndexes(exp.Tables[0].Indexes[0])
	exp.Tables[0].Columns[1].AddIndexes(exp.Tables[0].Indexes[0])
	exp.Tables[1].Columns[0].AddIndexes(exp.Tables[1].PrimaryKey)
	var s schema.Schema
	err := EvalHCLBytes([]byte(f), &s, nil)
	require.NoError(t, err)
	require.EqualValues(t, exp, &s)
}

func TestUnmarshalViews(t *testing.T) {
	f := `table "t1" {
  schema = schema.public
  column "id" {
    null = false
    type = int
  }
}
view "v1" {
 schema = schema.public
 as     = "SELECT * FROM t2 WHERE id IS NOT NULL"
}
view "v2" {
  schema = schema.public
  column "id" {
    null = false
    type = int
  }
  as      = "SELECT * FROM t3 WHERE id IS NOT NULL ORDER BY id"
  comment = "view comment"
}
view "v3" {
  schema     = schema.public
  as         = "SELECT * FROM v2 JOIN t1 USING (id)"
  depends_on = [view.v1, table.t1]
}
schema "public" {
}
`
	var (
		r   schema.Realm
		s   schema.Schema
		exp = schema.New("public").
			AddTables(
				schema.NewTable("t1").
					AddColumns(
						schema.NewIntColumn("id", "int"),
					),
			).
			AddViews(
				schema.NewView("v1", "SELECT * FROM t2 WHERE id IS NOT NULL"),
				schema.NewView("v2", "SELECT * FROM t3 WHERE id IS NOT NULL ORDER BY id").
					AddColumns(
						schema.NewIntColumn("id", "int"),
					).
					SetComment("view comment"),
			)
	)
	exp.AddViews(
		schema.NewView("v3", "SELECT * FROM v2 JOIN t1 USING (id)").
			AddDeps(exp.Views[0], exp.Tables[0]),
	)
	r.AddSchemas(exp)
	require.NoError(t, EvalHCLBytes([]byte(f), &s, nil))
	require.EqualValues(t, exp, &s)
}

func TestMarshalSpec_AutoIncrement(t *testing.T) {
	s := &schema.Schema{
		Name: "test",
		Tables: []*schema.Table{
			{
				Name: "users",
				Columns: []*schema.Column{
					{
						Name: "id",
						Type: &schema.ColumnType{Type: &schema.IntegerType{T: "int"}},
						Attrs: []schema.Attr{
							&AutoIncrement{},
						},
					},
				},
			},
		},
	}
	s.Tables[0].Schema = s
	buf, err := MarshalHCL(s)
	require.NoError(t, err)
	const expected = `table "users" {
  schema = schema.test
  column "id" {
    null           = false
    type           = int
    auto_increment = true
  }
}
schema "test" {
}
`
	require.EqualValues(t, expected, string(buf))
}

func TestMarshalSpec_IndexPredicate(t *testing.T) {
	s := &schema.Schema{
		Name: "test",
		Tables: []*schema.Table{
			{
				Name: "users",
				Columns: []*schema.Column{
					{
						Name: "id",
						Type: &schema.ColumnType{Type: &schema.IntegerType{T: "int"}},
						Attrs: []schema.Attr{
							&AutoIncrement{},
						},
					},
				},
			},
		},
	}
	s.Tables[0].Schema = s
	s.Tables[0].Schema = s
	s.Tables[0].Indexes = []*schema.Index{
		{
			Name:   "index",
			Table:  s.Tables[0],
			Unique: true,
			Parts: []*schema.IndexPart{
				{SeqNo: 0, C: s.Tables[0].Columns[0]},
			},
			Attrs: []schema.Attr{
				&IndexPredicate{P: "id <> 0"},
			},
		},
	}
	buf, err := MarshalHCL(s)
	require.NoError(t, err)
	const expected = `table "users" {
  schema = schema.test
  column "id" {
    null           = false
    type           = int
    auto_increment = true
  }
  index "index" {
    unique  = true
    columns = [column.id]
    where   = "id <> 0"
  }
}
schema "test" {
}
`
	require.EqualValues(t, expected, string(buf))
}

func TestTypes(t *testing.T) {
	for _, tt := range []struct {
		typeExpr string
		expected schema.Type
	}{
		{
			typeExpr: "integer(10)",
			expected: &schema.IntegerType{T: "integer"},
		},
		{
			typeExpr: "int(10)",
			expected: &schema.IntegerType{T: "int"},
		},
		{
			typeExpr: `sql("custom")`,
			expected: &UserDefinedType{T: "custom"},
		},
		{
			typeExpr: "tinyint(10)",
			expected: &schema.IntegerType{T: "tinyint"},
		},
		{
			typeExpr: "smallint(10)",
			expected: &schema.IntegerType{T: "smallint"},
		},
		{
			typeExpr: "mediumint(10)",
			expected: &schema.IntegerType{T: "mediumint"},
		},
		{
			typeExpr: "bigint(10)",
			expected: &schema.IntegerType{T: "bigint"},
		},
		{
			typeExpr: "unsigned_big_int(10)",
			expected: &schema.IntegerType{T: "unsigned big int"},
		},
		{
			typeExpr: "int2(10)",
			expected: &schema.IntegerType{T: "int2"},
		},
		{
			typeExpr: "int8(10)",
			expected: &schema.IntegerType{T: "int8"},
		},
		{
			typeExpr: "uint64",
			expected: &schema.IntegerType{T: "uint64"},
		},
		{
			typeExpr: "real",
			expected: &schema.FloatType{T: "real"},
		},
		{
			typeExpr: "double",
			expected: &schema.FloatType{T: "double"},
		},
		{
			typeExpr: "double_precision",
			expected: &schema.FloatType{T: "double precision"},
		},
		{
			typeExpr: "float(10)",
			expected: &schema.FloatType{T: "float"},
		},
		{
			typeExpr: "text(10)",
			expected: &schema.StringType{T: "text", Size: 10},
		},
		{
			typeExpr: "character(10)",
			expected: &schema.StringType{T: "character", Size: 10},
		},
		{
			typeExpr: "varchar(10)",
			expected: &schema.StringType{T: "varchar", Size: 10},
		},
		{
			typeExpr: "varying_character",
			expected: &schema.StringType{T: "varying character"},
		},
		{
			typeExpr: "nchar(10)",
			expected: &schema.StringType{T: "nchar", Size: 10},
		},
		{
			typeExpr: "native_character",
			expected: &schema.StringType{T: "native character"},
		},
		{
			typeExpr: "nvarchar(10)",
			expected: &schema.StringType{T: "nvarchar", Size: 10},
		},
		{
			typeExpr: "clob(10)",
			expected: &schema.StringType{T: "clob", Size: 10},
		},
		{
			typeExpr: "blob(10)",
			expected: &schema.BinaryType{T: "blob"},
		},
		{
			typeExpr: "numeric(10)",
			expected: &schema.DecimalType{T: "numeric", Precision: 10},
		},
		{
			typeExpr: "decimal(10,5)",
			expected: &schema.DecimalType{T: "decimal", Precision: 10, Scale: 5},
		},
		{
			typeExpr: "boolean",
			expected: &schema.BoolType{T: "boolean"},
		},
		{
			typeExpr: "date",
			expected: &schema.TimeType{T: "date"},
		},
		{
			typeExpr: "datetime",
			expected: &schema.TimeType{T: "datetime"},
		},
		{
			typeExpr: "json",
			expected: &schema.JSONType{T: "json"},
		},
		{
			typeExpr: "uuid",
			expected: &schema.UUIDType{T: "uuid"},
		},
		{
			typeExpr: "jsonb",
			expected: &schema.JSONType{T: "jsonb"},
		},
	} {
		t.Run(tt.typeExpr, func(t *testing.T) {
			var test schema.Schema
			doc := fmt.Sprintf(`table "test" {
	schema = schema.test
	column "test" {
		null = false
		type = %s
	}
}
schema "test" {
}
`, tt.typeExpr)
			err := EvalHCLBytes([]byte(doc), &test, nil)
			require.NoError(t, err)
			colspec := test.Tables[0].Columns[0]
			require.EqualValues(t, tt.expected, colspec.Type.Type)
			spec, err := MarshalHCL(&test)
			require.NoError(t, err)
			var after schema.Schema
			err = EvalHCLBytes(spec, &after, nil)
			require.NoError(t, err)
			require.EqualValues(t, tt.expected, after.Tables[0].Columns[0].Type.Type)
		})
	}
}

func TestMarshalSpec_TableOptions(t *testing.T) {
	s := schema.New("test").
		AddTables(
			schema.NewTable("users").
				AddColumns(
					schema.NewIntColumn("id", "int"),
				).
				AddAttrs(
					&WithoutRowID{},
					&Strict{},
				),
		)
	s.Tables[0].SetSchema(s)
	buf, err := MarshalHCL(s)
	require.NoError(t, err)
	const expected = `table "users" {
  schema = schema.test
  column "id" {
    null = false
    type = int
  }
  without_rowid = true
  strict        = true
}
schema "test" {
}
`
	require.EqualValues(t, expected, string(buf))
}

func TestInputVars(t *testing.T) {
	spectest.TestInputVars(t, EvalHCL)
}
