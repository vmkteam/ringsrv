package db

import (
	"context"
	"fmt"

	"github.com/vmkteam/ringsrv/pkg/ring/dbq"

	"github.com/go-pg/pg/v10"
	"github.com/go-pg/pg/v10/orm"
	"github.com/go-pg/pg/v10/types"
)

// rawRows is the orm model an arbitrary SELECT is scanned into. go-pg's own map
// model loses the column order, and the answer is an array of arrays in that
// order — so this keeps the columns as the server described them and the values
// as go-pg reads them by type OID. Anything unrecognised, numeric included,
// stays the server's text.
type rawRows struct {
	max  int
	cols []types.ColumnInfo
	rows [][]any
	cur  []any
	skip bool
	// budget is the most the rows may weigh on the wire; past it the query is
	// cancelled and overflow says so. A row cap alone lets one wide row fill
	// the process.
	budget   int
	read     int
	overflow bool
	cancel   context.CancelFunc
}

var _ orm.HooklessModel = (*rawRows)(nil)

func (r *rawRows) Init() error {
	r.cols, r.rows, r.cur = nil, nil, nil
	return nil
}

// NextColumnScanner is called once per row. Past the cap the row is still
// read off the wire — the protocol has no way to stop — but not kept.
func (r *rawRows) NextColumnScanner() orm.ColumnScanner {
	r.skip = r.max > 0 && len(r.rows) >= r.max
	r.cur = nil
	return r
}

func (r *rawRows) AddColumnScanner(orm.ColumnScanner) error {
	if !r.skip {
		r.rows = append(r.rows, r.cur)
	}
	return nil
}

// ScanColumn receives every column of every row. The column description arrives
// with the first row, which is why a result of no rows has no columns: go-pg
// hands the description to the model only alongside a value.
func (r *rawRows) ScanColumn(col types.ColumnInfo, rd types.Reader, n int) error {
	if len(r.rows) == 0 && int(col.Index) == len(r.cols) {
		r.cols = append(r.cols, col)
	}
	if n > 0 {
		r.read += n
	}
	if r.budget > 0 && r.read > r.budget && !r.overflow {
		// The cancel reaches the server through go-pg's cancel request;
		// what is left of the result is not read.
		r.overflow = true
		r.cancel()
	}
	if r.skip || r.overflow {
		return nil
	}
	if r.cur == nil {
		r.cur = make([]any, 0, len(r.cols))
	}
	if n == -1 {
		r.cur = append(r.cur, nil)
		return nil
	}
	v, err := types.ReadColumnValue(col, rd, n)
	if err != nil {
		return err
	}
	if raw, ok := v.(types.RawValue); ok {
		v = raw.Value
	}
	r.cur = append(r.cur, v)
	return nil
}

// result renders the rows for the domain, naming the types.
func (r *rawRows) result(typeName func(int32) string) *dbq.Result {
	out := &dbq.Result{Columns: make([]dbq.Column, len(r.cols)), Rows: r.rows}
	for i, col := range r.cols {
		out.Columns[i] = dbq.Column{Name: col.Name, Type: typeName(col.DataType)}
	}
	if out.Rows == nil {
		out.Rows = [][]any{}
	}
	return out
}

// resolveTypes asks pg_type for the names of OIDs the client has not seen
// before — one query per new set, none once the schema's types are known.
func (c *Client) resolveTypes(ctx context.Context, tx *pg.Tx, rows *rawRows) error {
	var unknown []int32
	c.mu.Lock()
	for _, col := range rows.cols {
		if _, ok := c.typeNames[col.DataType]; !ok {
			unknown = append(unknown, col.DataType)
		}
	}
	c.mu.Unlock()
	if len(unknown) == 0 {
		return nil
	}

	var pairs []struct {
		OID  int32  `pg:"oid"`
		Name string `pg:"name"`
	}
	if _, err := tx.QueryContext(ctx, &pairs,
		"SELECT oid, format_type(oid, NULL) AS name FROM pg_type WHERE oid = ANY(?)", pg.Array(unknown)); err != nil {
		return fmt.Errorf("resolve column types: %w", err)
	}
	c.mu.Lock()
	for _, p := range pairs {
		c.typeNames[p.OID] = p.Name
	}
	c.mu.Unlock()
	return nil
}

// typeName is the cached name of a type OID, or the OID itself when the
// lookup never saw it.
func (c *Client) typeName(oid int32) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if name, ok := c.typeNames[oid]; ok {
		return name
	}
	return fmt.Sprintf("oid:%d", oid)
}
