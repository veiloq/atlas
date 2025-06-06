// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

//go:build !ent

package mysql

import (
	"encoding/hex"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/veiloq/atlas/sql/internal/sqlx"
	"github.com/veiloq/atlas/sql/schema"
)

// DefaultDiff provides basic diffing capabilities for MySQL dialects.
// Note, it is recommended to call Open, create a new Driver and use its
// Differ when a database connection is available.
var DefaultDiff schema.Differ = &sqlx.Diff{DiffDriver: &diff{conn: noConn}}

// A diff provides a MySQL implementation for sqlx.DiffDriver.
type diff struct {
	*conn
	// charset to collation mapping.
	// See, internal directory.
	ch2co, co2ch struct {
		sync.Once
		v   map[string]string
		err error
	}
}

// SupportChange reports if the change is supported by the differ.
func (*diff) SupportChange(c schema.Change) bool {
	switch c.(type) {
	case *schema.RenameConstraint:
		return false
	}
	return true
}

// SchemaAttrDiff returns a changeset for migrating schema attributes from one state to the other.
func (d *diff) SchemaAttrDiff(from, to *schema.Schema) []schema.Change {
	var (
		topAttr []schema.Attr
		changes []schema.Change
	)
	if from.Realm != nil {
		topAttr = from.Realm.Attrs
	}
	// Charset change.
	if change := d.charsetChange(from.Attrs, topAttr, to.Attrs); change != noChange {
		changes = append(changes, change)
	}
	// Collation change.
	if change := d.collationChange(from.Attrs, topAttr, to.Attrs); change != noChange {
		changes = append(changes, change)
	}
	return changes
}

// RealmObjectDiff returns a changeset for migrating realm (database) objects
// from one state to the other. For example, adding extensions or users.
func (*diff) RealmObjectDiff(_, _ *schema.Realm) ([]schema.Change, error) {
	return nil, nil
}

// SchemaObjectDiff returns a changeset for migrating schema objects from
// one state to the other.
func (*diff) SchemaObjectDiff(_, _ *schema.Schema, _ *schema.DiffOptions) ([]schema.Change, error) {
	return nil, nil
}

// TableAttrDiff returns a changeset for migrating table attributes from one state to the other.
func (d *diff) TableAttrDiff(from, to *schema.Table, opts *schema.DiffOptions) ([]schema.Change, error) {
	var changes []schema.Change
	if change := d.autoIncChange(from.Attrs, to.Attrs); change != noChange {
		changes = append(changes, change)
	}
	if change := sqlx.CommentDiff(from.Attrs, to.Attrs); change != nil {
		changes = append(changes, change)
	}
	if change := d.charsetChange(from.Attrs, from.Schema.Attrs, to.Attrs); change != noChange {
		changes = append(changes, change)
	}
	if change := d.collationChange(from.Attrs, from.Schema.Attrs, to.Attrs); change != noChange {
		changes = append(changes, change)
	}
	if change := d.engineChange(from.Attrs, to.Attrs); change != noChange {
		changes = append(changes, change)
	}
	if change := d.systemVerChange(from.Attrs, to.Attrs); change != noChange {
		changes = append(changes, change)
	}
	if !d.SupportsCheck() && sqlx.Has(to.Attrs, &schema.Check{}) {
		return nil, fmt.Errorf("version %q does not support CHECK constraints", d.V)
	}
	// For MariaDB, we skip JSON CHECK constraints that were created by the databases,
	// or by Atlas for older versions. These CHECK constraints (inlined on the columns)
	// also cannot be dropped using "DROP CONSTRAINTS", but can be modified and dropped
	// using "MODIFY COLUMN".
	var checks []schema.Change
	for _, c := range sqlx.CheckDiffMode(from, to, opts.Mode, func(c1, c2 *schema.Check) bool {
		return enforced(c1.Attrs) == enforced(c2.Attrs)
	}) {
		drop, ok := c.(*schema.DropCheck)
		if !ok || !strings.HasPrefix(drop.C.Expr, "json_valid") {
			checks = append(checks, c)
			continue
		}
		// Generated CHECK have the form of "json_valid(`<column>`)"
		// and named as the column.
		if _, ok := to.Column(drop.C.Name); !ok {
			checks = append(checks, c)
		}
	}
	return append(changes, checks...), nil
}

// ColumnChange returns the schema changes (if any) for migrating one column to the other.
func (d *diff) ColumnChange(fromT *schema.Table, from, to *schema.Column, _ *schema.DiffOptions) (schema.Change, error) {
	change := sqlx.CommentChange(from.Attrs, to.Attrs)
	if from.Type.Null != to.Type.Null {
		change |= schema.ChangeNull
	}
	changed, err := d.typeChanged(from, to)
	if err != nil {
		return sqlx.NoChange, err
	}
	if changed {
		change |= schema.ChangeType
	}
	if changed, err = d.defaultChanged(from, to); err != nil {
		return sqlx.NoChange, err
	}
	if changed {
		change |= schema.ChangeDefault
	}
	if changed, err = d.generatedChanged(from, to); err != nil {
		return sqlx.NoChange, err
	}
	if changed {
		change |= schema.ChangeGenerated
	}
	if changed, err = d.columnCharsetChanged(fromT, from, to); err != nil {
		return sqlx.NoChange, err
	}
	if changed {
		change |= schema.ChangeCharset
	}
	if changed, err = d.columnCollateChanged(fromT, from, to); err != nil {
		return sqlx.NoChange, err
	}
	if changed {
		change |= schema.ChangeCollate
	}
	if change.Is(schema.NoChange) {
		return sqlx.NoChange, nil
	}
	return &schema.ModifyColumn{
		Change: change,
		From:   from,
		To:     to,
	}, nil
}

// IsGeneratedIndexName reports if the index name was generated by the database.
func (d *diff) IsGeneratedIndexName(_ *schema.Table, idx *schema.Index) bool {
	// Auto-generated index names for functional/expression indexes. See.
	// mysql-server/sql/sql_table.cc#add_functional_index_to_create_list
	const f = "functional_index"
	switch {
	case d.SupportsIndexExpr() && idx.Name == f:
		return true
	case d.SupportsIndexExpr() && strings.HasPrefix(idx.Name+"_", f):
		i, err := strconv.ParseInt(strings.TrimLeft(idx.Name, idx.Name+"_"), 10, 64)
		return err == nil && i > 1
	case len(idx.Parts) == 0 || idx.Parts[0].C == nil:
		return false
	}
	// Unnamed INDEX or UNIQUE constraints are named by
	// the first index-part (as column or part of it).
	// For example, "c", "c_2", "c_3", etc.
	switch name := idx.Parts[0].C.Name; {
	case idx.Name == name:
		return true
	case strings.HasPrefix(idx.Name, name+"_"):
		i, err := strconv.ParseInt(strings.TrimPrefix(idx.Name, name+"_"), 10, 64)
		return err == nil && i > 1
	default:
		return false
	}
}

// IndexAttrChanged reports if the index attributes were changed.
func (*diff) IndexAttrChanged(from, to []schema.Attr) bool {
	if indexType(from).T != indexType(to).T {
		return true
	}
	var (
		fromP, toP     IndexParser
		fromHas, toHas = sqlx.Has(from, &fromP), sqlx.Has(to, &toP)
	)
	return fromHas != toHas || (fromHas && fromP.P != toP.P)
}

// IndexPartAttrChanged reports if the index-part attributes (collation or prefix) were changed.
func (*diff) IndexPartAttrChanged(fromI, toI *schema.Index, i int) bool {
	var s1, s2 SubPart
	return sqlx.Has(fromI.Parts[i].Attrs, &s1) != sqlx.Has(toI.Parts[i].Attrs, &s2) || s1.Len != s2.Len
}

// ReferenceChanged reports if the foreign key referential action was changed.
func (*diff) ReferenceChanged(from, to schema.ReferenceOption) bool {
	// According to MySQL docs, foreign key constraints are checked
	// immediately, so NO ACTION is the same as RESTRICT. Specifying
	// RESTRICT (or NO ACTION) is the same as omitting the ON DELETE
	// or ON UPDATE clause.
	if from == "" || from == schema.Restrict {
		from = schema.NoAction
	}
	if to == "" || to == schema.Restrict {
		to = schema.NoAction
	}
	return from != to
}

// ForeignKeyAttrChanged reports if any of the foreign-key attributes were changed.
func (*diff) ForeignKeyAttrChanged(_, _ []schema.Attr) bool {
	return false
}

// Normalize implements the sqlx.Normalizer interface.
func (d *diff) Normalize(from, to *schema.Table, opts *schema.DiffOptions) error {
	if opts.Mode.Is(schema.DiffModeNormalized) {
		return nil // already normalized
	}
	indexes := make([]*schema.Index, 0, len(from.Indexes))
	for _, idx := range from.Indexes {
		// MySQL requires that foreign key columns be indexed; Therefore, if the child
		// table is defined on non-indexed columns, an index is automatically created
		// to satisfy the constraint.
		// Therefore, if no such key was defined on the desired state, the diff will
		// recommend dropping it on migration. Therefore, we fix it by dropping it from
		// the current state manually.
		if _, ok := to.Index(idx.Name); ok || !keySupportsFK(from, idx) {
			indexes = append(indexes, idx)
		}
	}
	from.Indexes = indexes

	// In case the "current" state was inspected (or loaded) with the collation/charset attributes,
	// but there are not found on the desired state, detect what are the default settings for the
	// desired state of the table (based on database default) to avoid proposing unnecessary changes.
	if sqlx.Has(from.Attrs, &schema.Collation{}) {
		if err := d.defaultCollate(&to.Attrs); err != nil {
			return err
		}
	}
	if sqlx.Has(from.Attrs, &schema.Charset{}) {
		if err := d.defaultCharset(&to.Attrs); err != nil {
			return err
		}
	}
	return nil
}

// FindTable implements the DiffDriver.TableFinder method in order to provide
// tables lookup that respect the "lower_case_table_names" system variable.
func (d *diff) FindTable(s *schema.Schema, t1 *schema.Table) (*schema.Table, error) {
	switch d.lcnames {
	// In mode 0: tables are stored as specified, and comparisons are case-sensitive.
	case 0:
		t2, ok := s.Table(t1.Name)
		if !ok {
			return nil, &schema.NotExistError{Err: fmt.Errorf("table %q was not found", t1.Name)}
		}
		return t2, nil
	// In mode 1: the table are stored in lowercase, but they are still
	// returned on inspection, because comparisons are not case-sensitive.
	// In mode 2: the tables are stored as given but compared in lowercase.
	// This option is not supported by Linux-based systems.
	case 1, 2:
		var matches []*schema.Table
		for _, t2 := range s.Tables {
			if strings.ToLower(t2.Name) == strings.ToLower(t1.Name) {
				matches = append(matches, t2)
			}
		}
		switch n := len(matches); n {
		case 0:
			return nil, &schema.NotExistError{Err: fmt.Errorf("table %q was not found", t1.Name)}
		case 1:
			return matches[0], nil
		default:
			return nil, fmt.Errorf("%d matches found for table %q", n, t1.Name)
		}
	default:
		return nil, fmt.Errorf("unsupported 'lower_case_table_names' mode: %d", d.lcnames)
	}
}

// collationChange returns the schema change for migrating the collation if
// it was changed, and it is not the default attribute inherited from its parent.
func (*diff) collationChange(from, top, to []schema.Attr) schema.Change {
	var fromC, topC, toC schema.Collation
	switch fromHas, topHas, toHas := sqlx.Has(from, &fromC), sqlx.Has(top, &topC), sqlx.Has(to, &toC); {
	case !fromHas && !toHas:
	case !fromHas:
		return &schema.AddAttr{
			A: &toC,
		}
	case !toHas:
		// There is no way to DROP a COLLATE that was configured on the table,
		// and it is not the default. Therefore, we use ModifyAttr and give it
		// the inherited (and default) collation from schema or server.
		if topHas && fromC.V != topC.V {
			return &schema.ModifyAttr{
				From: &fromC,
				To:   &topC,
			}
		}
	case fromC.V != toC.V:
		return &schema.ModifyAttr{
			From: &fromC,
			To:   &toC,
		}
	}
	return noChange
}

// engineChange returns the schema change for migrating the table engine in case
// it was changed.
func (*diff) engineChange(from, to []schema.Attr) schema.Change {
	var fromE, toE Engine
	switch fromHas, toHas := sqlx.Has(from, &fromE), sqlx.Has(to, &toE); {
	// Both engines are defined but different.
	case fromHas && toHas && strings.ToLower(fromE.V) != strings.ToLower(toE.V):
		return &schema.ModifyAttr{
			From: &fromE,
			To:   &toE,
		}
	// If the engine attribute has been removed from the desired state (e.g., HCL), and the current state
	// is not the default, we change the engine to InnoDB (the default for MySQL, unless configured otherwise).
	case fromHas && !toHas && !fromE.Default && strings.ToLower(fromE.V) != strings.ToLower(EngineInnoDB):
		return &schema.ModifyAttr{
			From: &fromE,
			To:   &Engine{V: EngineInnoDB, Default: true},
		}
	// In case the engine attribute was added to the desired state (e.g., HCL)
	// and it is not the default, we modify the engine to the desired value.
	case !fromHas && toHas && !toE.Default && strings.ToLower(fromE.V) != strings.ToLower(EngineInnoDB):
		return &schema.ModifyAttr{
			From: &Engine{V: EngineInnoDB, Default: true},
			To:   &toE,
		}
	}
	return noChange
}

// systemVerChange returns the schema change for migrating the system versioning
// attributes if it was changed.
func (d *diff) systemVerChange(from, to []schema.Attr) schema.Change {
	switch fromHas, toHas := sqlx.Has(from, &SystemVersioned{}), sqlx.Has(to, &SystemVersioned{}); {
	case fromHas && !toHas:
		return &schema.DropAttr{A: &SystemVersioned{}}
	case !fromHas && toHas:
		return &schema.AddAttr{A: &SystemVersioned{}}
	default:
		return noChange
	}
}

// charsetChange returns the schema change for migrating the collation if
// it was changed, and it is not the default attribute inherited from its parent.
func (*diff) charsetChange(from, top, to []schema.Attr) schema.Change {
	var fromC, topC, toC schema.Charset
	switch fromHas, topHas, toHas := sqlx.Has(from, &fromC), sqlx.Has(top, &topC), sqlx.Has(to, &toC); {
	case !fromHas && !toHas:
	case !fromHas:
		return &schema.AddAttr{
			A: &toC,
		}
	case !toHas:
		// There is no way to DROP a CHARSET that was configured on the table,
		// and it is not the default. Therefore, we use ModifyAttr and give it
		// the inherited (and default) collation from schema or server.
		if topHas && fromC.V != topC.V {
			return &schema.ModifyAttr{
				From: &fromC,
				To:   &topC,
			}
		}
	case fromC.V != toC.V:
		return &schema.ModifyAttr{
			From: &fromC,
			To:   &toC,
		}
	}
	return noChange
}

// columnCharsetChange indicates if there is a change to the column charset.
func (d *diff) columnCharsetChanged(fromT *schema.Table, from, to *schema.Column) (bool, error) {
	if err := d.defaultCharset(&to.Attrs); err != nil {
		return false, err
	}
	var (
		fromC, topC, toC       schema.Charset
		fromHas, topHas, toHas = sqlx.Has(from.Attrs, &fromC), sqlx.Has(fromT.Attrs, &topC), sqlx.Has(to.Attrs, &toC)
	)
	// Column was updated with custom CHARSET that was dropped.
	// Hence, we should revert to the one defined on the table.
	return fromHas && !toHas && topHas && fromC.V != topC.V ||
		// Custom CHARSET was added to the column. Hence,
		// Does not match the one defined in the table.
		!fromHas && toHas && topHas && toC.V != topC.V ||
		// CHARSET was explicitly changed.
		fromHas && toHas && fromC.V != toC.V, nil

}

// columnCollateChanged indicates if there is a change to the column charset.
func (d *diff) columnCollateChanged(fromT *schema.Table, from, to *schema.Column) (bool, error) {
	if err := d.defaultCollate(&to.Attrs); err != nil {
		return false, err
	}
	var (
		fromC, topC, toC       schema.Collation
		fromHas, topHas, toHas = sqlx.Has(from.Attrs, &fromC), sqlx.Has(fromT.Attrs, &topC), sqlx.Has(to.Attrs, &toC)
	)
	// Column was updated with custom COLLATE that was dropped.
	// Hence, we should revert to the one defined on the table.
	return fromHas && !toHas && topHas && fromC.V != topC.V ||
		// Custom COLLATE was added to the column. Hence,
		// Does not match the one defined in the table.
		!fromHas && toHas && topHas && toC.V != topC.V ||
		// COLLATE was explicitly changed.
		fromHas && toHas && fromC.V != toC.V, nil

}

// autoIncChange returns the schema change for changing the AUTO_INCREMENT
// attribute in case it is not the default.
func (*diff) autoIncChange(from, to []schema.Attr) schema.Change {
	var fromA, toA AutoIncrement
	switch fromHas, toHas := sqlx.Has(from, &fromA), sqlx.Has(to, &toA); {
	// Ignore if the AUTO_INCREMENT attribute was dropped from the desired schema.
	case fromHas && !toHas:
	// The AUTO_INCREMENT exists in the desired schema, and may not exist in the inspected one.
	// This can happen because older versions of MySQL (< 8.0) stored the AUTO_INCREMENT counter
	// in main memory (not persistent), and the value is reset on process restart for empty tables.
	case toA.V > 1 && toA.V > fromA.V:
		// Suggest a diff only if the desired value is greater than the inspected one,
		// because this attribute cannot be maintained in users schema and used to set
		// up only the initial value.
		return &schema.ModifyAttr{
			From: &fromA,
			To:   &toA,
		}
	}
	return noChange
}

// indexType returns the index type from its attribute.
// The default type is BTREE if no type was specified.
func indexType(attr []schema.Attr) *IndexType {
	t := &IndexType{T: IndexTypeBTree}
	if sqlx.Has(attr, t) {
		t.T = strings.ToUpper(t.T)
	}
	return t
}

// enforced returns the ENFORCED attribute for the CHECK
// constraint. A CHECK is ENFORCED if not state otherwise.
func enforced(attr []schema.Attr) bool {
	if e := (Enforced{}); sqlx.Has(attr, &e) {
		return e.V
	}
	return true
}

// noChange describes a zero change.
var noChange struct{ schema.Change }

func (d *diff) typeChanged(from, to *schema.Column) (bool, error) {
	fromT, toT := from.Type.Type, to.Type.Type
	if fromT == nil || toT == nil {
		return false, fmt.Errorf("mysql: missing type information for column %q", from.Name)
	}
	if reflect.TypeOf(fromT) != reflect.TypeOf(toT) {
		return true, nil
	}
	var changed bool
	switch fromT := fromT.(type) {
	case *BitType, *schema.BinaryType, *schema.BoolType, *schema.DecimalType, *schema.FloatType,
		*schema.JSONType, *schema.StringType, *schema.SpatialType, *schema.TimeType, *schema.UUIDType, *NetworkType:
		ft, err := FormatType(fromT)
		if err != nil {
			return false, err
		}
		tt, err := FormatType(toT)
		if err != nil {
			return false, err
		}
		changed = ft != tt
	case *schema.EnumType:
		toT := toT.(*schema.EnumType)
		changed = !sqlx.ValuesEqual(fromT.Values, toT.Values)
	case *schema.IntegerType:
		toT := toT.(*schema.IntegerType)
		// MySQL v8.0.19 dropped both display-width
		// and zerofill from the information schema.
		if d.SupportsDisplayWidth() {
			ft, _, _, err := parseColumn(fromT.T)
			if err != nil {
				return false, err
			}
			tt, _, _, err := parseColumn(toT.T)
			if err != nil {
				return false, err
			}
			fromT.T, toT.T = ft[0], tt[0]
		}
		changed = fromT.T != toT.T || fromT.Unsigned != toT.Unsigned
	case *SetType:
		toT := toT.(*SetType)
		changed = !sqlx.ValuesEqual(fromT.Values, toT.Values)
	default:
		return false, &sqlx.UnsupportedTypeError{Type: fromT}
	}
	return changed, nil
}

// defaultChanged reports if the default value of a column was changed.
func (d *diff) defaultChanged(from, to *schema.Column) (bool, error) {
	d1, ok1 := sqlx.DefaultValue(from)
	d2, ok2 := sqlx.DefaultValue(to)
	if ok1 != ok2 {
		return true, nil
	}
	if d1 == d2 {
		return false, nil
	}
	switch from.Type.Type.(type) {
	case *schema.BinaryType:
		a, err1 := binValue(d1)
		b, err2 := binValue(d2)
		if err1 != nil || err2 != nil {
			return true, nil
		}
		return !equalsStringValues(a, b), nil
	case *schema.BoolType:
		a, err1 := boolValue(d1)
		b, err2 := boolValue(d2)
		if err1 == nil && err2 == nil {
			return a != b, nil
		}
		return false, nil
	case *schema.IntegerType:
		return !d.equalIntValues(d1, d2), nil
	case *schema.FloatType, *schema.DecimalType:
		return !d.equalFloatValues(d1, d2), nil
	case *schema.EnumType, *SetType, *schema.StringType:
		return !equalsStringValues(d1, d2), nil
	case *schema.TimeType:
		x1 := strings.ToLower(strings.Trim(d1, "' ()"))
		x2 := strings.ToLower(strings.Trim(d2, "' ()"))
		return !equalsStringValues(x1, x2), nil
	default:
		x1 := strings.Trim(d1, "'")
		x2 := strings.Trim(d2, "'")
		return x1 != x2, nil
	}
}

// generatedChanged reports if the generated expression of a column was changed.
func (*diff) generatedChanged(from, to *schema.Column) (bool, error) {
	var (
		fromX, toX     schema.GeneratedExpr
		fromHas, toHas = sqlx.Has(from.Attrs, &fromX), sqlx.Has(to.Attrs, &toX)
	)
	if !fromHas && !toHas || fromHas && toHas && sqlx.MayWrap(fromX.Expr) == sqlx.MayWrap(toX.Expr) && storedOrVirtual(fromX.Type) == storedOrVirtual(toX.Type) {
		return false, nil
	}
	// Checking validity of the change is done
	// by the planner (checkChangeGenerated).
	return true, nil
}

// equalIntValues report if the 2 int default values are ~equal.
// Note that default expression are not supported atm.
func (d *diff) equalIntValues(x1, x2 string) bool {
	x1 = strings.ToLower(strings.Trim(x1, "' "))
	x2 = strings.ToLower(strings.Trim(x2, "' "))
	if x1 == x2 {
		return true
	}
	d1, err := strconv.ParseInt(x1, 10, 64)
	if err != nil {
		// Numbers are rounded down to their nearest integer.
		f, err := strconv.ParseFloat(x1, 64)
		if err != nil {
			return false
		}
		d1 = int64(f)
	}
	d2, err := strconv.ParseInt(x2, 10, 64)
	if err != nil {
		// Numbers are rounded down to their nearest integer.
		f, err := strconv.ParseFloat(x2, 64)
		if err != nil {
			return false
		}
		d2 = int64(f)
	}
	return d1 == d2
}

// equalFloatValues report if the 2 float default values are ~equal.
// Note that default expression are not supported atm.
func (d *diff) equalFloatValues(x1, x2 string) bool {
	x1 = strings.ToLower(strings.Trim(x1, "' "))
	x2 = strings.ToLower(strings.Trim(x2, "' "))
	if x1 == x2 {
		return true
	}
	d1, err := strconv.ParseFloat(x1, 64)
	if err != nil {
		return false
	}
	d2, err := strconv.ParseFloat(x2, 64)
	if err != nil {
		return false
	}
	return d1 == d2
}

// equalsStringValues report if the 2 string default values are
// equal after dropping their quotes.
func equalsStringValues(x1, x2 string) bool {
	a, err1 := sqlx.Unquote(x1)
	b, err2 := sqlx.Unquote(x2)
	return a == b && err1 == nil && err2 == nil
}

// boolValue returns the MySQL boolean value from the given string (if it is known).
func boolValue(x string) (bool, error) {
	switch x {
	case "1", "'1'", "TRUE", "true":
		return true, nil
	case "0", "'0'", "FALSE", "false":
		return false, nil
	default:
		return false, fmt.Errorf("mysql: unknown value: %q", x)
	}
}

// binValue returns the MySQL binary value from the given string (if it is known).
func binValue(x string) (string, error) {
	if !isHex(x) {
		return x, nil
	}
	d, err := hex.DecodeString(x[2:])
	if err != nil {
		return x, err
	}
	return string(d), nil
}

// keySupportsFK reports if the index key was created automatically by MySQL
// to support the constraint. See sql/sql_table.cc#find_fk_supporting_key.
func keySupportsFK(t *schema.Table, idx *schema.Index) bool {
	if _, ok := t.ForeignKey(idx.Name); ok {
		return true
	}
search:
	for _, fk := range t.ForeignKeys {
		if len(fk.Columns) != len(idx.Parts) {
			continue
		}
		for i, c := range fk.Columns {
			if idx.Parts[i].C == nil || idx.Parts[i].C.Name != c.Name {
				continue search
			}
		}
		return true
	}
	return false
}

// defaultCollate appends the default COLLATE to the attributes in case a
// custom character-set was defined for the element and the COLLATE was not.
func (d *diff) defaultCollate(attrs *[]schema.Attr) error {
	var charset schema.Charset
	if !sqlx.Has(*attrs, &charset) || sqlx.Has(*attrs, &schema.Collation{}) {
		return nil
	}
	d.ch2co.Do(func() {
		d.ch2co.v, d.ch2co.err = d.CharsetToCollate(d.ExecQuerier)
	})
	if d.ch2co.err != nil {
		return d.ch2co.err
	}
	if v, ok := d.ch2co.v[charset.V]; ok {
		// If charset is known, use its default collation.
		schema.ReplaceOrAppend(attrs, &schema.Collation{V: v})
	}
	return nil
}

// defaultCharset appends the default CHARSET to the attributes in case a
// custom collation was defined for the element and the CHARSET was not.
func (d *diff) defaultCharset(attrs *[]schema.Attr) error {
	var collate schema.Collation
	if !sqlx.Has(*attrs, &collate) || sqlx.Has(*attrs, &schema.Charset{}) {
		return nil
	}
	d.co2ch.Do(func() {
		d.co2ch.v, d.co2ch.err = d.CollateToCharset(d.ExecQuerier)
	})
	if d.co2ch.err != nil {
		return d.co2ch.err
	}
	if v, ok := d.co2ch.v[collate.V]; ok {
		// If collation is known, use its default charset.
		schema.ReplaceOrAppend(attrs, &schema.Charset{V: v})
	}
	return nil
}

func (*diff) ViewAttrChanges(_, _ *schema.View) []schema.Change {
	return nil // Not implemented.
}
