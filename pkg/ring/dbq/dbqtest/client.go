// Package dbqtest is a scripted database client for the tests above the
// drivers: the domain's manager and the tools that call it. The drivers
// themselves are tested against live databases in their own packages.
package dbqtest

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/vmkteam/ringsrv/pkg/ring/dbq"
)

// Client answers what it was told to. Every field may be changed between
// calls through the setters, which is how a test walks a target from
// unreachable to proven.
type Client struct {
	mu       sync.Mutex
	proof    dbq.Proof
	proveErr error
	proves   int
	// ProveDelay makes a probe take time, so a test can have several
	// callers meet the same one.
	ProveDelay time.Duration
	query      func(req dbq.Statement) (*dbq.Result, error)
	// TableList and Schemas are what the introspection answers; a table
	// absent from Schemas is unknown.
	TableList []dbq.Table
	Schemas   map[string]*dbq.Schema
	// VisibleScope is what Scope answers; ScopeErr makes the server
	// unanswerable on that question alone, which is how a test sees an
	// empty list explained without one.
	VisibleScope dbq.Scope
	ScopeErr     error
}

// New makes a client that proves read-only and answers queries with fn.
func New(fn func(req dbq.Statement) (*dbq.Result, error)) *Client {
	return &Client{proof: dbq.Proof{ReadOnly: true}, query: fn}
}

// SetProof scripts the next probes: err means the server could not be asked.
func (c *Client) SetProof(proof dbq.Proof, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.proof, c.proveErr = proof, err
}

// SetQuery scripts the next queries.
func (c *Client) SetQuery(fn func(req dbq.Statement) (*dbq.Result, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.query = fn
}

// Proves is how many times the probe ran.
func (c *Client) Proves() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.proves
}

func (c *Client) Query(_ context.Context, req dbq.Statement) (*dbq.Result, error) {
	c.mu.Lock()
	fn := c.query
	c.mu.Unlock()
	if fn == nil {
		return &dbq.Result{Rows: [][]any{}}, nil
	}
	return fn(req)
}

func (c *Client) Tables(context.Context) ([]dbq.Table, error) { return c.TableList, nil }

func (c *Client) Introspect(_ context.Context, table string) (*dbq.Schema, error) {
	if s, ok := c.Schemas[table]; ok {
		return s, nil
	}
	return nil, fmt.Errorf("%w: %s", dbq.ErrUnknownTable, table)
}

func (c *Client) Scope(context.Context) (dbq.Scope, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ScopeErr != nil {
		return dbq.Scope{}, c.ScopeErr
	}
	return c.VisibleScope, nil
}

func (c *Client) ProveReadOnly(context.Context) (dbq.Proof, error) {
	c.mu.Lock()
	c.proves++
	proof, err, delay := c.proof, c.proveErr, c.ProveDelay
	c.mu.Unlock()
	time.Sleep(delay)
	return proof, err
}

func (c *Client) Close() error { return nil }

// Rows scripts a query that answers n rows of (id, name), enough to see a
// cap and a redaction.
func Rows(n int) func(dbq.Statement) (*dbq.Result, error) {
	return func(req dbq.Statement) (*dbq.Result, error) {
		res := &dbq.Result{Columns: []dbq.Column{{Name: "id", Type: "bigint"}, {Name: "name", Type: "text"}}, Rows: [][]any{}}
		for i := range min(n, req.MaxRows) {
			res.Rows = append(res.Rows, []any{int64(i + 1), fmt.Sprintf("user%d@example.com", i+1)})
		}
		return res, nil
	}
}
