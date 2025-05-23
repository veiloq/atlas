// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package sqlx

import (
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/veiloq/atlas/sql/schema"
)

// NoChange can be returned by DiffDriver methods
// to indicate that no change is needed.
var NoChange = schema.Change(nil)

type (
	// A Diff provides a generic schema.Differ for diffing schema elements.
	//
	// The DiffDriver is required for supporting database/dialect specific
	// diff capabilities, like diffing custom types or attributes.
	Diff struct {
		DiffDriver
	}

	// A DiffDriver wraps all required methods for diffing elements that may
	// have database-specific diff logic. See sql/schema/mysql/diff.go for an
	// implementation example.
	DiffDriver interface {
		// RealmObjectDiff returns a changeset for migrating realm (database) objects
		// from one state to the other. For example, adding extensions or users.
		RealmObjectDiff(from, to *schema.Realm) ([]schema.Change, error)

		// SchemaAttrDiff returns a changeset for migrating schema attributes
		// from one state to the other. For example, changing schema collation.
		SchemaAttrDiff(from, to *schema.Schema) []schema.Change

		// SchemaObjectDiff returns a changeset for migrating schema objects from
		// one state to the other. For example, changing schema custom types.
		SchemaObjectDiff(from, to *schema.Schema, _ *schema.DiffOptions) ([]schema.Change, error)

		// TableAttrDiff returns a changeset for migrating table attributes from
		// one state to the other. For example, dropping or adding a `CHECK` constraint.
		TableAttrDiff(from, to *schema.Table, _ *schema.DiffOptions) ([]schema.Change, error)

		// ViewAttrChanges returns the changes between the two view attributes.
		ViewAttrChanges(from, to *schema.View) []schema.Change

		// ColumnChange returns the schema changes (if any) for migrating one column to the other.
		ColumnChange(fromT *schema.Table, from, to *schema.Column, _ *schema.DiffOptions) (schema.Change, error)

		// IndexAttrChanged reports if the index attributes were changed.
		// For example, an index type or predicate (for partial indexes).
		IndexAttrChanged(from, to []schema.Attr) bool

		// IndexPartAttrChanged reports if the part's attributes at position "i"
		// were changed. For example, an index-part collation.
		IndexPartAttrChanged(from, to *schema.Index, i int) bool

		// IsGeneratedIndexName reports if the index name was generated by the database
		// for unnamed INDEX or UNIQUE constraints. In such cases, the Differ will look
		// for unnamed schema.Indexes on the desired state, before tagging the index as
		// a candidate for deletion.
		IsGeneratedIndexName(*schema.Table, *schema.Index) bool

		// ReferenceChanged reports if the foreign key referential action was
		// changed. For example, action was changed from RESTRICT to CASCADE.
		ReferenceChanged(from, to schema.ReferenceOption) bool

		// ForeignKeyAttrChanged reports if any of the foreign-key attributes were changed.
		ForeignKeyAttrChanged(from, to []schema.Attr) bool
	}

	// DropSchemaChanger is an optional interface allows DiffDriver to drop
	// schema objects before dropping the schema itself.
	DropSchemaChanger interface {
		DropSchemaChange(*schema.Schema) []schema.Change
	}

	// A Normalizer wraps the Normalize method for normalizing the from and to tables before
	// running diffing. The "from" usually represents the inspected database state (current),
	// and the second represents the desired state.
	//
	// If the DiffDriver implements the Normalizer interface, TableDiff normalizes its table
	// inputs before starting the diff process.
	Normalizer interface {
		Normalize(from, to *schema.Table, opts *schema.DiffOptions) error
	}

	// TableFinder wraps the FindTable method, providing more
	// control to the DiffDriver on how tables are matched.
	TableFinder interface {
		FindTable(*schema.Schema, *schema.Table) (*schema.Table, error)
	}

	// ChangesAnnotator is an optional interface allows DiffDriver to annotate
	// changes with additional driver-specific attributes before they are returned.
	ChangesAnnotator interface {
		AnnotateChanges([]schema.Change, *schema.DiffOptions) error
	}

	// ProcFuncsDiffer is an optional interface allows DiffDriver to diff
	// functions and procedures.
	ProcFuncsDiffer interface {
		// ProcFuncsDiff returns a changeset for migrating functions and procedures
		// from one schema state to the other.
		ProcFuncsDiff(from, to *schema.Schema, opts *schema.DiffOptions) ([]schema.Change, error)
	}

	// TriggerDiffer is an optional interface allows DiffDriver to diff triggers.
	TriggerDiffer interface {
		// TriggerDiff returns a changeset for migrating triggers from
		// one state to the other. For example, changing action time.
		TriggerDiff(from, to *schema.Trigger) ([]schema.Change, error)
	}

	// ChangeSupporter wraps the single SupportChange method.
	ChangeSupporter interface {
		// SupportChange can be implemented to tell the Differ if they support
		// a specific change type, or it should avoid suggesting it.
		SupportChange(schema.Change) bool
	}
)

// RealmDiff implements the schema.Differ for Realm objects and returns a list of changes
// that need to be applied in order to move a database from the current state to the desired.
func (d *Diff) RealmDiff(from, to *schema.Realm, options ...schema.DiffOption) ([]schema.Change, error) {
	var (
		changes schema.Changes
		opts    = schema.NewDiffOptions(options...)
	)
	// Realm-level objects.
	change, err := d.RealmObjectDiff(from, to)
	if err != nil {
		return nil, err
	}
	changes = opts.AddOrSkip(changes, change...)
	// Drop or modify schema.
	for _, s1 := range from.Schemas {
		s2, ok := to.Schema(s1.Name)
		if !ok {
			if ds, ok := d.DiffDriver.(DropSchemaChanger); ok {
				// The driver can drop other objects before dropping the schema.
				changes = opts.AddOrSkip(changes, ds.DropSchemaChange(s1)...)
			} else {
				changes = opts.AddOrSkip(changes, &schema.DropSchema{S: s1})
			}
			continue
		}
		change, err := d.schemaDiff(s1, s2, opts)
		if err != nil {
			return nil, err
		}
		changes = append(changes, change...)
	}
	// Add schemas.
	for _, s1 := range to.Schemas {
		if _, ok := from.Schema(s1.Name); ok {
			continue
		}
		changes = opts.AddOrSkip(changes, &schema.AddSchema{S: s1})
		for _, o := range s1.Objects {
			changes = opts.AddOrSkip(changes, &schema.AddObject{O: o})
		}
		for _, f := range s1.Funcs {
			changes = opts.AddOrSkip(changes, &schema.AddFunc{F: f})
		}
		for _, p := range s1.Procs {
			changes = opts.AddOrSkip(changes, &schema.AddProc{P: p})
		}
		for _, t := range s1.Tables {
			changes = opts.AddOrSkip(changes, addTableChange(t)...)
		}
		for _, v := range s1.Views {
			changes = opts.AddOrSkip(changes, addViewChange(v)...)
		}
	}
	return d.mayAnnotate(changes, opts)
}

// SchemaDiff implements the schema.Differ interface and returns a list of
// changes that need to be applied in order to move from one state to the other.
func (d *Diff) SchemaDiff(from, to *schema.Schema, options ...schema.DiffOption) ([]schema.Change, error) {
	opts := schema.NewDiffOptions(options...)
	changes, err := d.schemaDiff(from, to, opts)
	if err != nil {
		return nil, err
	}
	return d.mayAnnotate(changes, opts)
}

func (d *Diff) schemaDiff(from, to *schema.Schema, opts *schema.DiffOptions) ([]schema.Change, error) {
	if from.Name != to.Name {
		return nil, fmt.Errorf("mismatched schema names: %q != %q", from.Name, to.Name)
	}
	var changes []schema.Change
	// Drop or modify attributes (collations, charset, etc).
	if change := d.SchemaAttrDiff(from, to); len(change) > 0 {
		changes = opts.AddOrSkip(changes, &schema.ModifySchema{
			S:       to,
			Changes: change,
		})
	}
	// Add, drop or modify objects.
	change, err := d.SchemaObjectDiff(from, to, opts)
	if err != nil {
		return nil, err
	}
	changes = opts.AddOrSkip(changes, change...)

	// Drop or modify tables.
	for _, t1 := range from.Tables {
		switch t2, err := d.findTable(to, t1); {
		case schema.IsNotExistError(err):
			// Triggers should be dropped either by the driver or the database.
			changes = opts.AddOrSkip(changes, &schema.DropTable{T: t1})
		case err != nil:
			return nil, err
		default:
			if change, err := d.tableDiff(t1, t2, opts); err != nil {
				return nil, err
			} else if len(change) > 0 {
				changes = opts.AddOrSkip(changes, &schema.ModifyTable{T: t2, Changes: change})
			}
			if change, err := d.triggerDiff(t1, t2, t1.Triggers, t2.Triggers, opts); err != nil {
				return nil, err
			} else {
				changes = append(changes, change...)
			}
		}
	}
	changes = d.fixRenames(changes)
	// Add tables.
	for _, t1 := range to.Tables {
		switch _, err := d.findTable(from, t1); {
		case schema.IsNotExistError(err):
			changes = opts.AddOrSkip(changes, addTableChange(t1)...)
		case err != nil:
			return nil, err
		}
	}

	// Drop or modify views.
	for _, v1 := range from.Views {
		v2, ok := findView(to, v1)
		if !ok {
			// Changing a view to materialized (and vice versa)
			// generates a drop and add.
			changes = opts.AddOrSkip(changes, &schema.DropView{V: v1})
			continue
		}
		if change, err := d.viewDiff(v1, v2, opts); err != nil {
			return nil, err
		} else {
			changes = append(changes, change...)
		}
		if change, err := d.triggerDiff(v1, v2, v1.Triggers, v2.Triggers, opts); err != nil {
			return nil, err
		} else {
			changes = append(changes, change...)
		}
	}
	// Add views.
	for _, v1 := range to.Views {
		if _, ok := findView(from, v1); !ok {
			changes = opts.AddOrSkip(changes, addViewChange(v1)...)
		}
	}
	// Add, drop and modify functions and procedures.
	if pf, ok := d.DiffDriver.(ProcFuncsDiffer); ok {
		change, err := pf.ProcFuncsDiff(from, to, opts)
		if err != nil {
			return nil, err
		}
		changes = append(changes, change...)
	}
	return changes, nil
}

// TableDiff implements the schema.TableDiffer interface and returns a list of
// changes that need to be applied in order to move from one state to the other.
func (d *Diff) TableDiff(from, to *schema.Table, options ...schema.DiffOption) ([]schema.Change, error) {
	opts := schema.NewDiffOptions(options...)
	if from.Name != to.Name {
		return nil, fmt.Errorf("mismatched table names: %q != %q", from.Name, to.Name)
	}
	changes, err := d.tableDiff(from, to, opts)
	if err != nil {
		return nil, err
	}
	if change, err := d.triggerDiff(from, to, from.Triggers, to.Triggers, opts); err != nil {
		return nil, err
	} else {
		changes = append(changes, change...)
	}
	return d.mayAnnotate(changes, opts)
}

// tableDiff implements the table diffing but skips the table name check.
func (d *Diff) tableDiff(from, to *schema.Table, opts *schema.DiffOptions) ([]schema.Change, error) {
	// tableDiff can be called with non-identical
	// names without affecting the diff process.
	if name := from.Name; name != to.Name {
		from.Name = to.Name
		defer func() { from.Name = name }()
	}
	// Normalizing tables before starting the diff process.
	if n, ok := d.DiffDriver.(Normalizer); ok {
		if err := n.Normalize(from, to, opts); err != nil {
			return nil, err
		}
	}
	var changes []schema.Change
	// Drop or modify attributes (collations, checks, etc).
	change, err := d.TableAttrDiff(from, to, opts)
	if err != nil {
		return nil, err
	}
	changes = append(changes, change...)

	// Drop, add or modify columns.
	if change, err = d.columnDiff(from, to, opts); err != nil {
		return nil, err
	}
	changes = append(changes, change...)

	// Primary-key and index changes.
	changes = append(changes, d.pkDiff(from, to, opts)...)
	if change, err = d.indexDiffT(from, to, opts); err != nil {
		return nil, err
	}
	changes = append(changes, change...)

	// Drop or modify foreign-keys.
	for _, fk1 := range from.ForeignKeys {
		fk2, ok := to.ForeignKey(fk1.Symbol)
		if !ok {
			changes = opts.AddOrSkip(changes, &schema.DropForeignKey{F: fk1})
			continue
		}
		if change := d.fkChange(fk1, fk2); change != schema.NoChange {
			changes = opts.AddOrSkip(changes, &schema.ModifyForeignKey{
				From:   fk1,
				To:     fk2,
				Change: change,
			})
		}
	}
	// Add foreign-keys.
	for _, fk1 := range to.ForeignKeys {
		if _, ok := from.ForeignKey(fk1.Symbol); !ok {
			changes = opts.AddOrSkip(changes, &schema.AddForeignKey{F: fk1})
		}
	}
	return changes, nil
}

func (d *Diff) mayAnnotate(changes []schema.Change, opts *schema.DiffOptions) ([]schema.Change, error) {
	r, ok := d.DiffDriver.(ChangesAnnotator)
	if ok {
		if err := r.AnnotateChanges(changes, opts); err != nil {
			return nil, err
		}
	}
	return changes, nil
}

// addTableChange returns the changeset for creating the table.
func addTableChange(t *schema.Table) []schema.Change {
	changes := make([]schema.Change, 0, 1+len(t.Triggers))
	changes = append(changes, &schema.AddTable{T: t})
	for _, r := range t.Triggers {
		changes = append(changes, &schema.AddTrigger{T: r})
	}
	return changes
}

// addViewChange returns the changeset for creating the view.
func addViewChange(v *schema.View) []schema.Change {
	changes := make([]schema.Change, 0, 1+len(v.Triggers))
	changes = append(changes, &schema.AddView{V: v})
	for _, r := range v.Triggers {
		changes = append(changes, &schema.AddTrigger{T: r})
	}
	return changes
}

// columnDiff returns the schema changes (if any) for migrating table columns.
func (d *Diff) columnDiff(from, to *schema.Table, opts *schema.DiffOptions) ([]schema.Change, error) {
	var all []schema.Change
	// Drop or modify columns.
	for _, c1 := range from.Columns {
		c2, ok := to.Column(c1.Name)
		if !ok {
			all = append(all, &schema.DropColumn{C: c1})
			continue
		}
		change, err := d.ColumnChange(from, c1, c2, opts)
		if err != nil {
			return nil, err
		}
		if change != NoChange {
			all = append(all, change)
		}
	}
	// Add columns.
	for _, c1 := range to.Columns {
		if _, ok := from.Column(c1.Name); !ok {
			all = append(all, &schema.AddColumn{
				C: c1,
			})
		}
	}
	var (
		err     error
		changes = make([]schema.Change, 0, len(all))
	)
	if all, err = d.askForColumns(from, all, opts); err != nil {
		return nil, err
	}
	for _, c := range all {
		changes = opts.AddOrSkip(changes, c)
	}
	return changes, nil
}

// pkDiff returns the schema changes (if any) for migrating table
// primary-key from current state to the desired state.
func (d *Diff) pkDiff(from, to *schema.Table, opts *schema.DiffOptions) (changes []schema.Change) {
	switch pk1, pk2 := from.PrimaryKey, to.PrimaryKey; {
	case pk1 == nil && pk2 != nil:
		changes = opts.AddOrSkip(changes, &schema.AddPrimaryKey{P: pk2})
	case pk1 != nil && pk2 == nil:
		changes = opts.AddOrSkip(changes, &schema.DropPrimaryKey{P: pk1})
	case pk1 != nil:
		change := d.indexChange(pk1, pk2)
		change &= ^schema.ChangeUnique
		switch c, ok := d.DiffDriver.(ChangeSupporter); {
		case change != schema.NoChange:
			changes = opts.AddOrSkip(changes, &schema.ModifyPrimaryKey{
				From: pk1, To: pk2, Change: change,
			})
		case (!ok || c.SupportChange((*schema.RenameConstraint)(nil))) &&
			pk1.Name != "" && pk2.Name != "" && pk1.Name != pk2.Name:
			changes = opts.AddOrSkip(changes, &schema.RenameConstraint{
				From: pk1, To: pk2,
			})
		}
	}
	return
}

// indexDiffT returns the schema changes (if any) for migrating table
// indexes from current state to the desired state.
func (d *Diff) indexDiffT(from, to *schema.Table, opts *schema.DiffOptions) ([]schema.Change, error) {
	var (
		all    []schema.Change
		exists = make(map[*schema.Index]bool)
	)
	// Drop or modify indexes.
	for _, idx1 := range from.Indexes {
		idx2, ok := to.Index(idx1.Name)
		// Found directly.
		if ok {
			if change := d.indexChange(idx1, idx2); change != schema.NoChange {
				all = append(all, &schema.ModifyIndex{
					From:   idx1,
					To:     idx2,
					Change: change,
				})
			}
			exists[idx2] = true
			continue
		}
		// Found indirectly.
		if d.IsGeneratedIndexName(from, idx1) {
			if idx2, ok := d.similarUnnamedIndex(to, idx1); ok {
				exists[idx2] = true
				continue
			}
		}
		// Not found.
		all = append(all, &schema.DropIndex{I: idx1})
	}
	// Add indexes.
	for _, idx := range to.Indexes {
		if exists[idx] {
			continue
		}
		if _, ok := from.Index(idx.Name); !ok {
			all = append(all, &schema.AddIndex{I: idx})
		}
	}
	var (
		err     error
		changes = make([]schema.Change, 0, len(all))
	)
	if all, err = d.askForIndexes(from.Name, all, opts); err != nil {
		return nil, err
	}
	for _, c := range all {
		changes = opts.AddOrSkip(changes, c)
	}
	return changes, nil
}

// viewDiff returns the schema changes (if any) for migrating view from
// current state to the desired state.
func (d *Diff) viewDiff(from, to *schema.View, opts *schema.DiffOptions) ([]schema.Change, error) {
	c1, err := d.indexDiffV(from, to, opts)
	if err != nil {
		return nil, err
	}
	c2, err := d.columnDiffV(from, to, opts)
	if err != nil {
		return nil, err
	}
	var changes []schema.Change
	if vs := append(d.ViewAttrChanges(from, to), append(c1, c2...)...); len(vs) > 0 || d.viewDefChanged(from, to) {
		changes = opts.AddOrSkip(changes, &schema.ModifyView{From: from, To: to, Changes: vs})
	}
	return changes, nil
}

// viewDefChanged checks if the view definition has changed.
// It allows the DiffDriver to override the default implementation.
func (d *Diff) viewDefChanged(v1 *schema.View, v2 *schema.View) bool {
	if vr, ok := d.DiffDriver.(interface {
		ViewDefChanged(v1, v2 *schema.View) bool
	}); ok {
		return vr.ViewDefChanged(v1, v2)
	}
	return BodyDefChanged(v1.Def, v2.Def)
}

// columnDiffV returns the schema changes (if any) for migrating view columns.
// Currently, only comment changes are supported.
func (d *Diff) columnDiffV(from, to *schema.View, opts *schema.DiffOptions) ([]schema.Change, error) {
	var changes []schema.Change
	for _, c1 := range from.Columns {
		c2, ok := to.Column(c1.Name)
		if !ok {
			continue
		}
		if change := CommentChange(c1.Attrs, c2.Attrs); change != schema.NoChange {
			changes = opts.AddOrSkip(changes, &schema.ModifyColumn{
				From:   c1,
				To:     c2,
				Change: change,
			})
		}
	}
	return changes, nil
}

// indexDiffV returns the schema changes (if any) for migrating view
// indexes from current state to the desired state.
func (d *Diff) indexDiffV(from, to *schema.View, opts *schema.DiffOptions) ([]schema.Change, error) {
	var (
		changes []schema.Change
		exists  = make(map[*schema.Index]bool)
	)
	// Drop or modify indexes.
	for _, idx1 := range from.Indexes {
		idx2, ok := to.Index(idx1.Name)
		if ok {
			if change := d.indexChange(idx1, idx2); change != schema.NoChange {
				changes = opts.AddOrSkip(changes, &schema.ModifyIndex{
					From:   idx1,
					To:     idx2,
					Change: change,
				})
			}
			exists[idx2] = true
			continue
		}
		// Not found.
		changes = opts.AddOrSkip(changes, &schema.DropIndex{I: idx1})
	}
	// Add indexes.
	for _, idx := range to.Indexes {
		if exists[idx] {
			continue
		}
		if _, ok := from.Index(idx.Name); !ok {
			changes = opts.AddOrSkip(changes, &schema.AddIndex{I: idx})
		}
	}
	return d.askForIndexes(from.Name, changes, opts)
}

// indexChange returns the schema changes (if any) for migrating one index to the other.
func (d *Diff) indexChange(from, to *schema.Index) schema.ChangeKind {
	var change schema.ChangeKind
	if from.Unique != to.Unique {
		change |= schema.ChangeUnique
	}
	if d.IndexAttrChanged(from.Attrs, to.Attrs) {
		change |= schema.ChangeAttr
	}
	change |= d.partsChange(from, to, nil)
	change |= CommentChange(from.Attrs, to.Attrs)
	return change
}

func (d *Diff) partsChange(fromI, toI *schema.Index, renames map[string]string) schema.ChangeKind {
	from, to := fromI.Parts, toI.Parts
	if len(from) != len(to) {
		return schema.ChangeParts
	}
	sort.Slice(to, func(i, j int) bool { return to[i].SeqNo < to[j].SeqNo })
	sort.Slice(from, func(i, j int) bool { return from[i].SeqNo < from[j].SeqNo })
	for i := range from {
		switch {
		case from[i].Desc != to[i].Desc || d.IndexPartAttrChanged(fromI, toI, i):
			return schema.ChangeParts
		case from[i].C != nil && to[i].C != nil:
			if from[i].C.Name != to[i].C.Name && renames[from[i].C.Name] != to[i].C.Name {
				return schema.ChangeParts
			}
		case from[i].X != nil && to[i].X != nil:
			x1, x2 := from[i].X.(*schema.RawExpr).X, to[i].X.(*schema.RawExpr).X
			if x1 != x2 && x1 != MayWrap(x2) {
				return schema.ChangeParts
			}
		default: // (C1 != nil) != (C2 != nil) || (X1 != nil) != (X2 != nil).
			return schema.ChangeParts
		}
	}
	return schema.NoChange
}

// fkChange returns the schema changes (if any) for migrating one index to the other.
func (d *Diff) fkChange(from, to *schema.ForeignKey) schema.ChangeKind {
	var change schema.ChangeKind
	switch {
	case from.RefTable.Name != to.RefTable.Name:
		change |= schema.ChangeRefTable | schema.ChangeRefColumn
	case len(from.RefColumns) != len(to.RefColumns):
		change |= schema.ChangeRefColumn
	default:
		for i := range from.RefColumns {
			if from.RefColumns[i].Name != to.RefColumns[i].Name {
				change |= schema.ChangeRefColumn
			}
		}
	}
	switch {
	case len(from.Columns) != len(to.Columns):
		change |= schema.ChangeColumn
	default:
		for i := range from.Columns {
			if from.Columns[i].Name != to.Columns[i].Name {
				change |= schema.ChangeColumn
			}
		}
	}
	if d.ReferenceChanged(from.OnUpdate, to.OnUpdate) {
		change |= schema.ChangeUpdateAction
	}
	if d.ReferenceChanged(from.OnDelete, to.OnDelete) {
		change |= schema.ChangeDeleteAction
	}
	if d.ForeignKeyAttrChanged(from.Attrs, to.Attrs) {
		change |= schema.ChangeAttr
	}
	return change
}

// similarUnnamedIndex searches for an unnamed index with the same index-parts in the table.
func (d *Diff) similarUnnamedIndex(t *schema.Table, idx1 *schema.Index) (*schema.Index, bool) {
	match := func(idx1, idx2 *schema.Index) bool {
		return idx1.Unique == idx2.Unique && d.partsChange(idx1, idx2, nil) == schema.NoChange
	}
	if f, ok := d.DiffDriver.(interface {
		FindGeneratedIndex(*schema.Table, *schema.Index) (*schema.Index, bool)
	}); ok {
		if idx2, ok := f.FindGeneratedIndex(t, idx1); ok && match(idx1, idx2) {
			return idx2, true
		}
	}
	for _, idx2 := range t.Indexes {
		if idx2.Name == "" && match(idx1, idx2) {
			return idx2, true
		}
	}
	return nil, false
}

func (d *Diff) findTable(s *schema.Schema, t1 *schema.Table) (*schema.Table, error) {
	if f, ok := d.DiffDriver.(TableFinder); ok {
		return f.FindTable(s, t1)
	}
	t2, ok := s.Table(t1.Name)
	if !ok {
		return nil, &schema.NotExistError{Err: fmt.Errorf("table %q was not found", t1.Name)}
	}
	return t2, nil
}

// CommentChange reports if the element comment was changed.
func CommentChange(from, to []schema.Attr) schema.ChangeKind {
	var c1, c2 schema.Comment
	if Has(from, &c1) != Has(to, &c2) || c1.Text != c2.Text {
		return schema.ChangeComment
	}
	return schema.NoChange
}

// Charset reports if the attribute contains the "charset" attribute,
// and it needs to be defined explicitly on the schema. This is true, in
// case the element charset is different from its parent charset.
func Charset(attr, parent []schema.Attr) (string, bool) {
	var c, p schema.Charset
	if Has(attr, &c) && (parent == nil || Has(parent, &p) && c.V != p.V) {
		return c.V, true
	}
	return "", false
}

// Collate reports if the attribute contains the "collation"/"collate" attribute,
// and it needs to be defined explicitly on the schema. This is true, in
// case the element collation is different from its parent collation.
func Collate(attr, parent []schema.Attr) (string, bool) {
	var c, p schema.Collation
	if Has(attr, &c) && (parent == nil || Has(parent, &p) && c.V != p.V) {
		return c.V, true
	}
	return "", false
}

var (
	attrsType   = reflect.TypeOf(([]schema.Attr)(nil))
	clausesType = reflect.TypeOf(([]schema.Clause)(nil))
	exprsType   = reflect.TypeOf(([]schema.Expr)(nil))
)

// AttrOr returns the first attribute of the given type,
// or the given default value.
func AttrOr[T schema.Attr](attrs []schema.Attr, t T) T {
	for _, attr := range attrs {
		if a, ok := attr.(T); ok {
			return a
		}
	}
	return t
}

// Has finds the first element in the elements list that
// matches target, and if so, sets target to that attribute
// value and returns true.
func Has(elements, target any) bool {
	ev := reflect.ValueOf(elements)
	if t := ev.Type(); t != attrsType && t != clausesType && t != exprsType {
		panic(fmt.Sprintf("unexpected elements type: %T", elements))
	}
	tv := reflect.ValueOf(target)
	if tv.Kind() != reflect.Ptr || tv.IsNil() {
		panic("target must be a non-nil pointer")
	}
	for i := 0; i < ev.Len(); i++ {
		idx := ev.Index(i)
		if idx.IsNil() {
			continue
		}
		if e := idx.Elem(); e.Type().AssignableTo(tv.Type()) {
			tv.Elem().Set(e.Elem())
			return true
		}
	}
	return false
}

// UnsupportedTypeError describes an unsupported type error.
type UnsupportedTypeError struct {
	schema.Type
}

func (e UnsupportedTypeError) Error() string {
	return fmt.Sprintf("unsupported type %T", e.Type)
}

// CommentDiff computes the comment diff between the 2 attribute list.
// Note that, the implementation relies on the fact that both PostgreSQL
// and MySQL treat empty comment as "no comment" and a way to clear comments.
func CommentDiff(from, to []schema.Attr) schema.Change {
	var fromC, toC schema.Comment
	switch fromHas, toHas := Has(from, &fromC), Has(to, &toC); {
	case !fromHas && !toHas:
	case !fromHas && toC.Text != "":
		return &schema.AddAttr{
			A: &toC,
		}
	case !toHas:
		// In MySQL, there is no way to DROP a comment. Instead, setting it to empty ('')
		// will remove it from INFORMATION_SCHEMA. We use the same approach in PostgreSQL,
		// because comments can be dropped either by setting them to NULL or empty string.
		// See: postgres/backend/commands/comment.c#CreateComments.
		return &schema.ModifyAttr{
			From: &fromC,
			To:   &toC,
		}
	default:
		v1, err1 := Unquote(fromC.Text)
		v2, err2 := Unquote(toC.Text)
		if err1 == nil && err2 == nil && v1 != v2 {
			return &schema.ModifyAttr{
				From: &fromC,
				To:   &toC,
			}
		}
	}
	return nil
}

// CheckDiffMode is like CheckDiff, but compares also expressions
// if the schema.DiffMode is equal to schema.DiffModeNormalized.
func CheckDiffMode(from, to *schema.Table, mode schema.DiffMode, compare ...func(c1, c2 *schema.Check) bool) []schema.Change {
	if !mode.Is(schema.DiffModeNormalized) {
		return checksSimilarDiff(from, to, compare...)
	}
	return ChecksDiff(from, to, func(c1, c2 *schema.Check) bool {
		if len(compare) == 1 && !compare[0](c1, c2) {
			return false
		}
		return c1.Expr == c2.Expr || MayWrap(c1.Expr) == MayWrap(c2.Expr)
	})
}

// ChecksDiff computes the change diff between the 2 tables.
func ChecksDiff(from, to *schema.Table, compare func(c1, c2 *schema.Check) bool) []schema.Change {
	var (
		changes    []schema.Change
		fromC, toC = checks(from.Attrs), checks(to.Attrs)
		compareTo  = func(c1 *schema.Check) func(c2 *schema.Check) bool {
			return func(c2 *schema.Check) bool {
				if c1.Name != "" && c2.Name != "" {
					// Only compare by name if both have a name.
					return c1.Name == c2.Name
				}
				return compare(c1, c2)
			}
		}
	)
	for _, c1 := range fromC {
		switch idx := slices.IndexFunc(toC, compareTo(c1)); {
		case idx == -1:
			changes = append(changes, &schema.DropCheck{
				C: c1,
			})
		case !compare(c1, toC[idx]):
			changes = append(changes, &schema.ModifyCheck{
				From: c1,
				To:   toC[idx],
			})
		}
	}
	for _, c1 := range toC {
		if !slices.ContainsFunc(fromC, compareTo(c1)) {
			changes = append(changes, &schema.AddCheck{
				C: c1,
			})
		}
	}
	return changes
}

// checksSimilarDiff computes the change diff between the 2 tables.
// Unlike ChecksDiff, it does not compare the constraint name, but
// determines if there is any similar constraint by its expression.
// This is an old implementation that is not used anymore by the CLI.
func checksSimilarDiff(from, to *schema.Table, compare ...func(c1, c2 *schema.Check) bool) []schema.Change {
	var changes []schema.Change
	// Drop or modify checks.
	for _, c1 := range checks(from.Attrs) {
		switch c2, ok := similarCheck(to.Attrs, c1); {
		case !ok:
			changes = append(changes, &schema.DropCheck{
				C: c1,
			})
		case len(compare) == 1 && !compare[0](c1, c2):
			changes = append(changes, &schema.ModifyCheck{
				From: c1,
				To:   c2,
			})
		}
	}
	// Add checks.
	for _, c1 := range checks(to.Attrs) {
		if _, ok := similarCheck(from.Attrs, c1); !ok {
			changes = append(changes, &schema.AddCheck{
				C: c1,
			})
		}
	}
	return changes
}

// checks extracts all constraints from table attributes.
func checks(attr []schema.Attr) (checks []*schema.Check) {
	for i := range attr {
		if c, ok := attr[i].(*schema.Check); ok {
			checks = append(checks, c)
		}
	}
	return checks
}

// similarCheck returns a CHECK by its constraints name or expression.
func similarCheck(attrs []schema.Attr, c *schema.Check) (*schema.Check, bool) {
	var byName, byExpr *schema.Check
	for i := 0; i < len(attrs) && (byName == nil || byExpr == nil); i++ {
		check, ok := attrs[i].(*schema.Check)
		if !ok {
			continue
		}
		if check.Name != "" && check.Name == c.Name {
			byName = check
		}
		if check.Expr == c.Expr || MayWrap(check.Expr) == MayWrap(c.Expr) {
			byExpr = check
		}
	}
	// Give precedence to constraint name.
	if byName != nil {
		return byName, true
	}
	if byExpr != nil {
		return byExpr, true
	}
	return nil, false
}

// Unquote single or double quotes.
func Unquote(s string) (string, error) {
	switch {
	case IsQuoted(s, '"'):
		return strconv.Unquote(s)
	case IsQuoted(s, '\''):
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'"), nil
	default:
		return s, nil
	}
}

// SingleQuote quotes the given string with single quote.
func SingleQuote(s string) (string, error) {
	switch {
	case IsQuoted(s, '\''):
		return s, nil
	case IsQuoted(s, '"'):
		v, err := strconv.Unquote(s)
		if err != nil {
			return "", err
		}
		s = v
		fallthrough
	default:
		return "'" + strings.ReplaceAll(s, "'", "''") + "'", nil
	}
}

// TrimViewExtra trims the extra unnecessary
// characters from the view definition.
func TrimViewExtra(s string) string {
	return strings.Trim(s, " \r\n\t;")
}

// BodyDefChanged reports if the body definition of a function, procedure, view, or
// trigger has changed. There is a small task here that normalizes the indentation,
// which might be added during inspection or by the user.
func BodyDefChanged(from, to string) bool {
	if from == to {
		return false // Exact match.
	}
	if from, to = TrimViewExtra(from), TrimViewExtra(to); from == to {
		return false // Match after trimming.
	}
	noident := func(v string) string {
		var b strings.Builder
		for i, s := range strings.Split(v, "\n") {
			if s = TrimViewExtra(s); s != "" && i > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(s)
		}
		return b.String()
	}
	return noident(from) != noident(to)
}

// findView finds the view by its name and its type.
func findView(s *schema.Schema, v1 *schema.View) (*schema.View, bool) {
	if v1.Materialized() {
		return s.Materialized(v1.Name)
	}
	return s.View(v1.Name)
}
