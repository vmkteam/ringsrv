// This file is the shape of a read-only SQL conversation as a client sees it,
// and the part of the domain a driver (pkg/db, pkg/chdb) imports so the Client
// interface can name these types.
//
// Nothing here knows about the catalogue, roles or tools: a client learns the
// SQL, the row cap, the timeout and who is asking, and answers with rows.

package dbq

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

// DefaultPoolSize is what a client opens when the catalogue names nothing: two
// keep an investigation from queueing behind itself without turning the instance
// into a load generator.
const DefaultPoolSize = 2

// Statement is one query, already checked and wrapped by the domain.
type Statement struct {
	SQL string
	// MaxRows is how many rows the client reads before it stops; the domain
	// asks for one more than it will return, to learn whether there were more.
	MaxRows int
	// MaxBytes is how much the client reads off the wire before it cancels and
	// answers ErrResultTooLarge: a row cap alone lets one wide row fill the
	// process. Zero means no budget.
	MaxBytes int
	// Timeout is set on the server (statement_timeout, max_execution_time) and
	// on the context the client is given.
	Timeout time.Duration
	// Caller is recorded by the server — application_name in PostgreSQL — so a
	// long query is attributable from the DBMS side too.
	Caller string
}

// Column is one column of a result: its name and the DBMS type as a string.
type Column struct {
	Name string
	Type string
}

// Result is what the DBMS returned, values as the driver produced them; the
// domain normalises them for JSON.
type Result struct {
	Columns []Column
	Rows    [][]any
}

// Table is one row of the table list.
type Table struct {
	Schema string
	Name   string
	// Kind is table, view or materialized view for PostgreSQL, the engine for
	// ClickHouse.
	Kind string
	// RowsEstimate comes from the planner's statistics or the engine's counters,
	// never from count(*).
	RowsEstimate int64
	Comment      string
}

// Schema describes one table.
type Schema struct {
	Table string
	// Schema is where the table was found, unabbreviated: Table drops the prefix
	// when the schema is first in the search path, and the pointer to the code
	// is resolved per schema. Empty for a driver without schemas.
	Schema       string
	Comment      string
	Columns      []ColumnInfo
	PrimaryKey   []string
	Relations    []Relation
	Checks       []string
	RowsEstimate int64
	// Engine, SortingKey and PartitionKey are ClickHouse's.
	Engine       string
	SortingKey   string
	PartitionKey string
}

// ColumnInfo is one column of a table.
type ColumnInfo struct {
	Name     string
	Type     string
	Nullable bool
	Comment  string
	// Enum lists the labels of a PostgreSQL enum type; empty otherwise.
	Enum []string
}

// Relation is a foreign key: this column references that table's column.
type Relation struct {
	Column     string
	References string
}

// Scope is what a connection can actually see, and the one question that tells
// an empty answer from a missing grant: an empty table list reads the same
// whether the database holds nothing, the role was granted nothing in it, or the
// catalogue named a base that is not there.
type Scope struct {
	// Database is the base the connection is on, as the server names it.
	Database string
	// Visible are the databases (ClickHouse) or schemas (PostgreSQL) this role
	// may read, whatever the catalogue declared. What is here and not declared
	// is drift; what is declared and missing here is a grant never given.
	Visible []string
}

// Proof is what the read-only probe found. ReadOnly false with a Reason means
// the server answered and the user can write — the one finding that refuses a
// start. An error from the probe means the server could not be asked.
type Proof struct {
	ReadOnly bool
	Reason   string
}

// Sentinel errors a client wraps so the domain can tell the failure apart
// without knowing the driver's own error types.
var (
	// ErrTimeout is a query the server or the client cut on time.
	ErrTimeout = errors.New("query timed out")
	// ErrUnknownTable is a query or an introspection of a table that is
	// not there.
	ErrUnknownTable = errors.New("unknown table")
	// ErrResultTooLarge is a result that went past Statement.MaxBytes and
	// was cancelled.
	ErrResultTooLarge = errors.New("result too large")
	// ErrNotReadOnly is a connection the client itself refused because the
	// user can write. It is the probe's verdict, not a fault of the
	// network, and a Proof carries it as ReadOnly = false.
	ErrNotReadOnly = errors.New("user is not read-only")
)

// NotReadOnlyError is ErrNotReadOnly with the probe's reason attached, the
// form a connection hook returns and a proof reads back.
type NotReadOnlyError struct{ Reason string }

func (e *NotReadOnlyError) Error() string        { return ErrNotReadOnly.Error() + ": " + e.Reason }
func (e *NotReadOnlyError) Is(target error) bool { return target == ErrNotReadOnly }

// WrapTimeout marks the timeouts every client meets the same way — the
// socket's read deadline and the context's — as ErrTimeout, and leaves any
// other error alone. A driver adds its own server-side code before calling
// it.
func WrapTimeout(err error) error {
	if err == nil {
		return nil
	}
	var nerr net.Error
	if (errors.As(err, &nerr) && nerr.Timeout()) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", ErrTimeout, err)
	}
	return err
}
