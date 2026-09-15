package chdb

import (
	"context"
	"fmt"
	"strings"

	"github.com/vmkteam/ringsrv/pkg/ring/dbq"
)

// Tables lists the target database from system.tables: the engine as the
// kind, total_rows as the estimate — never count() — and the
// comment the owner left.
func (c *Client) Tables(parent context.Context) ([]dbq.Table, error) {
	return under(parent, c.tables)
}

func (c *Client) tables(ctx context.Context) ([]dbq.Table, error) {
	rows, err := c.conn.Query(ctx,
		"SELECT name, engine, total_rows, comment FROM system.tables WHERE database = ? ORDER BY name", c.database)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []dbq.Table{}
	for rows.Next() {
		var t dbq.Table
		var total *uint64
		if err := rows.Scan(&t.Name, &t.Kind, &total, &t.Comment); err != nil {
			return nil, err
		}
		t.RowsEstimate = -1
		if total != nil {
			t.RowsEstimate = int64(*total)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Scope answers what this connection sees: the database it is on and every
// database the user may read. system.databases is already filtered by the
// server to what the user has a grant on, which is exactly the question —
// a base that is not in this list answers "unknown table" for every table
// it holds, and that reads like an empty base.
func (c *Client) Scope(parent context.Context) (dbq.Scope, error) {
	return under(parent, c.scope)
}

func (c *Client) scope(ctx context.Context) (dbq.Scope, error) {
	out := dbq.Scope{Database: c.database}
	rows, err := c.conn.Query(ctx, "SELECT name FROM system.databases ORDER BY name")
	if err != nil {
		return dbq.Scope{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return dbq.Scope{}, err
		}
		out.Visible = append(out.Visible, name)
	}
	if err := rows.Err(); err != nil {
		return dbq.Scope{}, err
	}
	return out, nil
}

// Introspect describes one table from system.tables and system.columns:
// engine, keys, the estimate, and every column with its type and comment.
// A missing table answers dbq.ErrUnknownTable.
func (c *Client) Introspect(parent context.Context, table string) (*dbq.Schema, error) {
	return under(parent, func(ctx context.Context) (*dbq.Schema, error) { return c.introspect(ctx, table) })
}

func (c *Client) introspect(ctx context.Context, table string) (*dbq.Schema, error) {
	database, name := c.database, table
	if d, n, ok := strings.Cut(table, "."); ok {
		database, name = d, n
	}

	out, err := c.tableHeader(ctx, database, name)
	if err != nil {
		return nil, err
	}
	if database != c.database {
		out.Table = database + "." + name
	}

	cols, err := c.conn.Query(ctx, `
SELECT name, type, comment FROM system.columns WHERE database = ? AND table = ? ORDER BY position`, database, name)
	if err != nil {
		return nil, err
	}
	defer cols.Close()
	for cols.Next() {
		var col dbq.ColumnInfo
		if err := cols.Scan(&col.Name, &col.Type, &col.Comment); err != nil {
			return nil, err
		}
		col.Nullable = strings.HasPrefix(col.Type, "Nullable(")
		out.Columns = append(out.Columns, col)
	}
	if err := cols.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// tableHeader reads the one row of system.tables a table has, or reports
// that it has none.
func (c *Client) tableHeader(ctx context.Context, database, name string) (*dbq.Schema, error) {
	rows, err := c.conn.Query(ctx, `
SELECT engine, total_rows, comment, sorting_key, partition_key, primary_key
FROM system.tables WHERE database = ? AND name = ?`, database, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if rowsErr := rows.Err(); rowsErr != nil {
			return nil, rowsErr
		}
		return nil, fmt.Errorf("%w: %s", dbq.ErrUnknownTable, name)
	}
	out := &dbq.Schema{Table: name, RowsEstimate: -1}
	var total *uint64
	var primaryKey string
	if scanErr := rows.Scan(&out.Engine, &total, &out.Comment, &out.SortingKey, &out.PartitionKey, &primaryKey); scanErr != nil {
		return nil, scanErr
	}
	if total != nil {
		out.RowsEstimate = int64(*total)
	}
	if primaryKey != "" {
		out.PrimaryKey = strings.Split(primaryKey, ", ")
	}
	return out, nil
}
