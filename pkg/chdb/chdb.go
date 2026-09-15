// Package chdb reads ClickHouse through the native protocol. It executes what
// the domain gives it: no writer, and no text check of its own.
//
// What it adds is the proof: the user's readonly and allow_ddl settings, its
// grants, and which query settings its profile lets it change. The last decides
// what travels with a query — a setting the profile declared READONLY fails
// every query that carries it, so only what the probe found changeable is sent.
package chdb

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/vmkteam/ringsrv/pkg/ring/dbq"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Options are what the catalogue entry becomes.
type Options struct {
	Addr     string
	Database string
	User     string
	Password string
	// MaxOpenConns is the number of connections and of queries in flight.
	MaxOpenConns int
	// Timeout bounds the socket read two seconds past the statement's own cap,
	// so a server that stopped answering is noticed; zero keeps the default.
	Timeout time.Duration
}

const (
	dialTimeout = 5 * time.Second

	// ClickHouse error codes the client tells apart.
	codeTimeoutExceeded = 159
	codeUnknownTable    = 60
)

// Client is one pool to one database.
type Client struct {
	conn     driver.Conn
	database string

	mu sync.Mutex
	// changeable is what the probe found the user may set per query; nil until
	// the first probe, and nothing is sent until then.
	changeable map[string]bool
}

// New opens the pool. clickhouse-go dials lazily: an unreachable server is
// a state the domain keeps, not an error here.
func New(o Options) (*Client, error) {
	maxOpen := o.MaxOpenConns
	if maxOpen <= 0 {
		maxOpen = dbq.DefaultPoolSize
	}
	opts := &clickhouse.Options{
		Addr:         []string{o.Addr},
		Auth:         clickhouse.Auth{Database: o.Database, Username: o.User, Password: o.Password},
		MaxOpenConns: maxOpen,
		DialTimeout:  dialTimeout,
	}
	if o.Timeout > 0 {
		opts.ReadTimeout = o.Timeout + 2*time.Second
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("chdb open: %w", err)
	}
	return &Client{conn: conn, database: o.Database}, nil
}

// Query runs one statement with the caps the profile lets through —
// max_execution_time, max_result_rows and max_result_bytes when the user may set
// them, the context's deadline always. Past MaxRows or MaxBytes the rest is left
// to the server's cancel, which closing the rows sends.
func (c *Client) Query(parent context.Context, req dbq.Statement) (*dbq.Result, error) {
	return under(parent, func(ctx context.Context) (*dbq.Result, error) {
		if settings := c.querySettings(req); len(settings) > 0 {
			ctx = clickhouse.Context(ctx, clickhouse.WithSettings(settings))
		}
		return c.read(ctx, req)
	})
}

// read runs the statement and collects up to MaxRows rows.
func (c *Client) read(ctx context.Context, req dbq.Statement) (*dbq.Result, error) {
	rows, err := c.conn.Query(ctx, req.SQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	colTypes := rows.ColumnTypes()
	out := &dbq.Result{Columns: make([]dbq.Column, len(colTypes)), Rows: [][]any{}}
	for i, ct := range colTypes {
		out.Columns[i] = dbq.Column{Name: ct.Name(), Type: ct.DatabaseTypeName()}
	}
	read := 0
	for (req.MaxRows <= 0 || len(out.Rows) < req.MaxRows) && rows.Next() {
		dst := make([]any, len(colTypes))
		for i, ct := range colTypes {
			dst[i] = newPtrFor(ct)
		}
		if err := rows.Scan(dst...); err != nil {
			return nil, err
		}
		row := make([]any, len(colTypes))
		for i, p := range dst {
			row[i] = derefAny(p)
			read += weight(row[i])
		}
		if req.MaxBytes > 0 && read > req.MaxBytes {
			return nil, fmt.Errorf("%w: more than %d bytes read", dbq.ErrResultTooLarge, req.MaxBytes)
		}
		out.Rows = append(out.Rows, row)
	}
	return out, rows.Err()
}

// weight is a cell's size for the byte budget: the length of what has one, a
// word for everything else. The budget guards against a column of documents,
// so an estimate is enough.
func weight(v any) int {
	switch t := v.(type) {
	case string:
		return len(t)
	case []byte:
		return len(t)
	case *string:
		if t != nil {
			return len(*t)
		}
	}
	return 8
}

// Query settings the client would like to send, and sends when the probe
// says the profile allows it.
const (
	settingMaxExecutionTime   = "max_execution_time"
	settingMaxResultRows      = "max_result_rows"
	settingMaxResultBytes     = "max_result_bytes"
	settingResultOverflowMode = "result_overflow_mode"
	settingReadonly           = "readonly"
	settingAllowDDL           = "allow_ddl"
)

// querySettings picks the settings for one query out of what the profile
// lets the user change.
func (c *Client) querySettings(req dbq.Statement) clickhouse.Settings {
	c.mu.Lock()
	changeable := c.changeable
	c.mu.Unlock()

	settings := clickhouse.Settings{}
	if changeable[settingMaxExecutionTime] && req.Timeout > 0 {
		settings[settingMaxExecutionTime] = max(1, int(req.Timeout.Seconds()))
	}
	if changeable[settingResultOverflowMode] && (req.MaxRows > 0 || req.MaxBytes > 0) {
		settings[settingResultOverflowMode] = "break"
		if changeable[settingMaxResultRows] && req.MaxRows > 0 {
			settings[settingMaxResultRows] = req.MaxRows
		}
		if changeable[settingMaxResultBytes] && req.MaxBytes > 0 {
			settings[settingMaxResultBytes] = req.MaxBytes
		}
	}
	if changeable[settingReadonly] {
		settings[settingReadonly] = 2
	}
	return settings
}

// Ping dials if need be and asks the server.
func (c *Client) Ping(parent context.Context) error {
	_, err := under(parent, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, c.conn.Ping(ctx)
	})
	return err
}

// under runs one call the way every call to the server is made: with the
// caller's deadline hidden from the driver and its failure classified.
func under[T any](parent context.Context, fn func(context.Context) (T, error)) (T, error) {
	ctx, cancel := detachDeadline(parent)
	defer cancel()
	out, err := fn(ctx)
	if err != nil {
		var zero T
		return zero, classifyUnder(parent, err)
	}
	return out, nil
}

// detachDeadline keeps the parent's cancellation and hides its deadline:
// clickhouse-go turns a deadline into a max_execution_time of its own, over
// whatever the query set, and a profile that pins that setting refuses the query
// (code 452). The cap goes as an explicit setting when the profile allows it;
// the deadline is enforced here, by cancelling.
func detachDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); !ok {
		return ctx, func() {}
	}
	out, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, cancel)
	return out, func() {
		stop()
		cancel()
	}
}

// Close releases the pool.
func (c *Client) Close() error { return c.conn.Close() }

// classifyUnder is classify for a call made under a detached deadline: the
// query saw a cancellation, but what the caller's context ran out of was
// time, and that is what the error has to say.
func classifyUnder(parent context.Context, err error) error {
	if errors.Is(parent.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", dbq.ErrTimeout, err)
	}
	return classify(err)
}

// classify marks what the domain tells apart: the server's
// max_execution_time (TIMEOUT_EXCEEDED) and a table that is not there
// (UNKNOWN_TABLE) by their codes, the socket's and the context's timeouts
// by dbq.WrapTimeout.
func classify(err error) error {
	var exc *clickhouse.Exception
	if errors.As(err, &exc) {
		switch exc.Code {
		case codeTimeoutExceeded:
			return fmt.Errorf("%w: %s", dbq.ErrTimeout, exc.Message)
		case codeUnknownTable:
			return fmt.Errorf("%w: %s", dbq.ErrUnknownTable, exc.Message)
		}
	}
	return dbq.WrapTimeout(err)
}
