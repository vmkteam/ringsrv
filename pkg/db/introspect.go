package db

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/vmkteam/ringsrv/pkg/ring/dbq"

	"github.com/go-pg/pg/v10"
	"github.com/go-pg/pg/v10/orm"
)

// Tables lists the relations of the configured schemas from pg_class: the
// kind, the planner's row estimate — never count(*) — and
// the comment the schema's owner left with COMMENT ON.
func (c *Client) Tables(ctx context.Context) ([]dbq.Table, error) {
	var rows []struct {
		Schema       string `pg:"schema"`
		Name         string `pg:"name"`
		Kind         string `pg:"kind"`
		RowsEstimate int64  `pg:"rows_estimate"`
		Comment      string `pg:"comment"`
	}
	err := c.inTx(ctx, c.timeout, "", func(tx *pg.Tx) error {
		_, err := tx.QueryContext(ctx, &rows, `
SELECT n.nspname AS schema, c.relname AS name,
       CASE c.relkind WHEN 'r' THEN 'table' WHEN 'p' THEN 'table' WHEN 'v' THEN 'view'
                      WHEN 'm' THEN 'materialized view' WHEN 'f' THEN 'foreign table' END AS kind,
       c.reltuples::bigint AS rows_estimate,
       COALESCE(obj_description(c.oid, 'pg_class'), '') AS comment
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p', 'v', 'm', 'f') AND n.nspname = ANY(?)
ORDER BY array_position(?, n.nspname::text), c.relname`, pg.Array(c.schemas), pg.Array(c.schemas))
		return err
	})
	if err != nil {
		return nil, classify(err)
	}
	out := make([]dbq.Table, len(rows))
	for i, r := range rows {
		out[i] = dbq.Table{Schema: r.Schema, Name: r.Name, Kind: r.Kind, RowsEstimate: r.RowsEstimate, Comment: r.Comment}
	}
	return out, nil
}

// Scope answers what this connection sees: the database it is on and every
// schema the role holds USAGE on. Without USAGE a schema's tables are
// invisible to pg_class-based listing and answer "relation does not exist"
// to a query — the same words a table that was never created gets, which is
// why the catalogue's list of schemas and the server's have to be
// comparable. The schemas PostgreSQL keeps for itself are left out: they
// are in every database and say nothing about this one.
func (c *Client) Scope(ctx context.Context) (dbq.Scope, error) {
	var rows []struct {
		Name string `pg:"name"`
	}
	err := c.inTx(ctx, c.timeout, "", func(tx *pg.Tx) error {
		_, err := tx.QueryContext(ctx, &rows, `
SELECT n.nspname AS name FROM pg_namespace n
WHERE has_schema_privilege(current_user, n.oid, 'USAGE')
  AND n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema'
ORDER BY n.nspname`)
		return err
	})
	if err != nil {
		return dbq.Scope{}, classify(err)
	}
	out := dbq.Scope{Database: c.database, Visible: make([]string, len(rows))}
	for i, r := range rows {
		out.Visible[i] = r.Name
	}
	return out, nil
}

// Introspect describes one table: columns with types, nullability and
// comments, enum labels, the primary key, foreign keys and checks — all from
// the catalogue tables, all bound as parameters. A bare name is looked up
// along the configured schemas in order; a name that is nowhere answers
// dbq.ErrUnknownTable.
func (c *Client) Introspect(ctx context.Context, table string) (*dbq.Schema, error) {
	schemas := c.schemas
	name := table
	if s, n, ok := strings.Cut(table, "."); ok {
		schemas, name = []string{s}, n
	}

	var rel struct {
		OID          int64  `pg:"oid"`
		Schema       string `pg:"schema"`
		Name         string `pg:"name"`
		Kind         string `pg:"kind"`
		RowsEstimate int64  `pg:"rows_estimate"`
		Comment      string `pg:"comment"`
	}
	var out *dbq.Schema
	err := c.inTx(ctx, c.timeout, "", func(tx *pg.Tx) error {
		_, err := tx.QueryOneContext(ctx, &rel, `
SELECT c.oid, n.nspname AS schema, c.relname AS name, c.relkind AS kind,
       c.reltuples::bigint AS rows_estimate,
       COALESCE(obj_description(c.oid, 'pg_class'), '') AS comment
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relname = ? AND n.nspname = ANY(?) AND c.relkind IN ('r', 'p', 'v', 'm', 'f')
ORDER BY array_position(?, n.nspname::text)
LIMIT 1`, name, pg.Array(schemas), pg.Array(schemas))
		if errors.Is(err, pg.ErrNoRows) {
			return fmt.Errorf("%w: %s", dbq.ErrUnknownTable, table)
		}
		if err != nil {
			return err
		}
		out = &dbq.Schema{Table: c.qualify(rel.Schema, rel.Name), Schema: rel.Schema, Comment: rel.Comment, RowsEstimate: rel.RowsEstimate}
		if err := c.columns(ctx, tx, rel.OID, out); err != nil {
			return err
		}
		return c.constraints(ctx, tx, rel.OID, out)
	})
	if errors.Is(err, dbq.ErrUnknownTable) {
		return nil, err
	}
	if err != nil {
		return nil, classify(err)
	}
	return out, nil
}

// qualify writes a table the way a query on this client names it: bare in
// the first schema of the search path, schema-qualified elsewhere.
func (c *Client) qualify(schema, name string) string {
	if schema == c.schemas[0] {
		return name
	}
	return schema + "." + name
}

func (c *Client) columns(ctx context.Context, q orm.DB, oid int64, out *dbq.Schema) error {
	var cols []struct {
		Name     string `pg:"name"`
		Type     string `pg:"type"`
		Nullable bool   `pg:"nullable"`
		Comment  string `pg:"comment"`
		EnumOID  int64  `pg:"enum_oid"`
	}
	if _, err := q.QueryContext(ctx, &cols, `
SELECT a.attname AS name, format_type(a.atttypid, a.atttypmod) AS type,
       NOT a.attnotnull AS nullable,
       COALESCE(col_description(a.attrelid, a.attnum), '') AS comment,
       CASE WHEN t.typtype = 'e' THEN t.oid ELSE 0 END AS enum_oid
FROM pg_attribute a
JOIN pg_type t ON t.oid = a.atttypid
WHERE a.attrelid = ? AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY a.attnum`, oid); err != nil {
		return err
	}
	// One query for every enum type the table uses, not one per column.
	var oids []int64
	for _, col := range cols {
		if col.EnumOID != 0 {
			oids = append(oids, col.EnumOID)
		}
	}
	enums := map[int64][]string{}
	if len(oids) > 0 {
		var rows []struct {
			OID    int64    `pg:"oid"`
			Labels []string `pg:"labels,array"`
		}
		if _, err := q.QueryContext(ctx, &rows, `
SELECT enumtypid AS oid, array_agg(enumlabel ORDER BY enumsortorder) AS labels
FROM pg_enum WHERE enumtypid = ANY(?) GROUP BY enumtypid`, pg.Array(oids)); err != nil {
			return err
		}
		for _, r := range rows {
			enums[r.OID] = r.Labels
		}
	}
	for _, col := range cols {
		info := dbq.ColumnInfo{Name: col.Name, Type: col.Type, Nullable: col.Nullable, Comment: col.Comment}
		if col.EnumOID != 0 {
			info.Enum = enums[col.EnumOID]
		}
		out.Columns = append(out.Columns, info)
	}
	return nil
}

func (c *Client) constraints(ctx context.Context, q orm.DB, oid int64, out *dbq.Schema) error {
	if _, err := q.QueryContext(ctx, pg.Scan(pg.Array(&out.PrimaryKey)), `
SELECT array_agg(a.attname ORDER BY k.ord)
FROM pg_index i
JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord) ON true
JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
WHERE i.indrelid = ? AND i.indisprimary`, oid); err != nil {
		return err
	}

	var fks []struct {
		Column     string `pg:"column"`
		References string `pg:"references"`
	}
	if _, err := q.QueryContext(ctx, &fks, `
SELECT a.attname AS column,
       CASE WHEN fn.nspname = 'public' THEN fc.relname ELSE fn.nspname || '.' || fc.relname END
         || '(' || fa.attname || ')' AS references
FROM pg_constraint con
JOIN pg_class fc ON fc.oid = con.confrelid
JOIN pg_namespace fn ON fn.oid = fc.relnamespace
JOIN LATERAL unnest(con.conkey, con.confkey) AS k(attnum, fattnum) ON true
JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = k.attnum
JOIN pg_attribute fa ON fa.attrelid = con.confrelid AND fa.attnum = k.fattnum
WHERE con.conrelid = ? AND con.contype = 'f'
ORDER BY con.conname, k.attnum`, oid); err != nil {
		return err
	}
	for _, fk := range fks {
		out.Relations = append(out.Relations, dbq.Relation{Column: fk.Column, References: fk.References})
	}

	if _, err := q.QueryContext(ctx, pg.Scan(pg.Array(&out.Checks)), `
SELECT array_agg(pg_get_constraintdef(oid) ORDER BY conname)
FROM pg_constraint WHERE conrelid = ? AND contype = 'c'`, oid); err != nil {
		return err
	}
	return nil
}
