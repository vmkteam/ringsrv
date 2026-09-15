// Package db reads foreign PostgreSQL databases through go-pg. The tables are
// somebody else's and are only ever read: every connection sets
// default_transaction_read_only and proves the user cannot write, and every
// statement runs alone in a READ ONLY transaction that is rolled back, with a
// statement timeout and the caller's name.
//
// The package knows the SQL, the caps and who is asking (dbq.Statement); the
// catalogue and the roles stay above it.
package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/vmkteam/ringsrv/pkg/ring/dbq"

	"github.com/go-pg/pg/v10"
	"github.com/go-pg/pg/v10/orm"
	"github.com/vmkteam/embedlog"
)

// Options are what the catalogue entry becomes.
type Options struct {
	Addr     string
	Database string
	User     string
	Password string
	// PoolSize is the number of connections and of queries in flight.
	PoolSize int
	// Timeout is the statement timeout of the introspection queries and the
	// ceiling of the socket read.
	Timeout time.Duration
	// Schemas are put into search_path, in order, and are what Tables lists.
	// Empty means public.
	Schemas []string
	// AppName is the application_name of the pool; each query overrides it with
	// the caller's.
	AppName string
	// Logger receives what go-pg would otherwise print to stderr.
	Logger embedlog.Logger
}

const (
	dialTimeout          = 5 * time.Second
	sqlstateTimeout      = "57014" // query_canceled: statement_timeout fired
	sqlstateUnknownTable = "42P01" // undefined_table
	defaultSchema        = "public"
)

// Client is one pool to one database.
type Client struct {
	db      *pg.DB
	schemas []string
	// database is the base this pool is connected to, kept so an answer can say
	// where it looked without asking the server.
	database string
	// searchPath is Schemas quoted and joined, computed once.
	searchPath string
	timeout    time.Duration

	mu        sync.Mutex
	typeNames map[int32]string
}

// New opens the pool. Nothing is dialled until the first use: an unreachable
// database is not an error here but a state the domain keeps.
func New(o Options) *Client {
	schemas := o.Schemas
	if len(schemas) == 0 {
		schemas = []string{defaultSchema}
	}
	quoted := make([]string, len(schemas))
	for i, s := range schemas {
		// Bare identifiers by the catalogue's validation; the doubling is
		// defence in depth.
		quoted[i] = `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	c := &Client{
		schemas: schemas, database: o.Database, searchPath: strings.Join(quoted, ", "),
		timeout: o.Timeout, typeNames: map[int32]string{},
	}

	poolSize := o.PoolSize
	if poolSize <= 0 {
		poolSize = dbq.DefaultPoolSize
	}
	setLogger(o.Logger)
	c.db = pg.Connect(&pg.Options{
		Addr:            o.Addr,
		User:            o.User,
		Password:        o.Password,
		Database:        o.Database,
		ApplicationName: o.AppName,
		PoolSize:        poolSize,
		DialTimeout:     dialTimeout,
		ReadTimeout:     o.Timeout + 2*time.Second,
		WriteTimeout:    dialTimeout,
		OnConnect:       c.onConnect,
	})
	return c
}

var loggerOnce sync.Once

// setLogger routes go-pg's own log lines into the structured log, once for the
// process: go-pg keeps one logger for every pool.
func setLogger(l embedlog.Logger) {
	if l.Log() == nil {
		return
	}
	loggerOnce.Do(func() { pg.SetLogger(pgLogger{Logger: l}) })
}

type pgLogger struct{ embedlog.Logger }

func (l pgLogger) Printf(ctx context.Context, format string, v ...any) {
	l.Print(ctx, "go-pg", "msg", fmt.Sprintf(format, v...))
}

// onConnect makes every connection read-only and proves it; a connection that
// fails the proof is closed and the pool never hands it out. The search path is
// a property of the client, so it is set here rather than per transaction, and
// pg.Safe keeps the formatter from quoting the already-quoted schemas again.
func (c *Client) onConnect(ctx context.Context, cn *pg.Conn) error {
	if _, err := cn.ExecContext(ctx, "SET default_transaction_read_only = on"); err != nil {
		return err
	}
	if _, err := cn.ExecContext(ctx, "SET search_path = ?", pg.Safe(c.searchPath)); err != nil {
		return err
	}
	proof, err := prove(ctx, cn)
	if err != nil {
		return err
	}
	if !proof.ReadOnly {
		return &dbq.NotReadOnlyError{Reason: proof.Reason}
	}
	return nil
}

// reachable is the set of roles the connected user may act as: itself and every
// role it is a member of. 'MEMBER' rather than 'USAGE' on purpose — a NOINHERIT
// membership grants nothing until SET ROLE, and SET ROLE is one statement away.
//
// The probes below read the catalogs directly rather than the grants views: the
// views hide a NOINHERIT membership, while the ACL columns of pg_class and
// pg_attribute are visible whatever the views filter. Refusing membership
// outright, as this used to, forced a choice between per-database grants and no
// proof at all.
const reachable = "pg_has_role(current_user, %s, 'MEMBER')"

// nonSystem excludes the schemas PostgreSQL writes itself: it grants PUBLIC
// an UPDATE on pg_settings, which is how SET works, not a way to write.
const nonSystem = "n.nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast')"

// heldByReachable matches an ACL entry that one of the reachable roles holds,
// counting PUBLIC — a table granted to everyone is granted to this role. The
// CASE is not decoration: grantee 0 means PUBLIC and is not a role oid, and
// only CASE guarantees that pg_has_role is not asked about it.
const heldByReachable = `a.privilege_type <> 'SELECT'
	   AND CASE WHEN a.grantee = 0 THEN true
	            ELSE pg_has_role(current_user, a.grantee, 'MEMBER') END`

// probes are what the server is asked about the current user, each with the
// answer that means "can write". They cover every role the user may become (see
// reachable). The last one refuses a database where dblink or a foreign-data
// wrapper is installed: their functions are executable by everyone and reach
// other hosts with credentials in the SQL.
var probes = []struct {
	query  string
	reason string
}{
	{
		"SELECT current_setting('default_transaction_read_only') <> 'on'",
		"default_transaction_read_only is off",
	},
	{
		"SELECT count(*) > 0 FROM pg_roles WHERE (rolsuper OR rolcreaterole OR rolcreatedb OR rolbypassrls) AND " +
			fmt.Sprintf(reachable, "oid"),
		"role, or one it may become, is a superuser, may create roles or databases, or bypasses row security",
	},
	{
		// The predefined roles carry their powers in the server's code, not
		// in any ACL, so the grants below would never see them.
		"SELECT count(*) > 0 FROM pg_roles WHERE rolname IN ('pg_write_all_data', 'pg_write_server_files', 'pg_execute_server_program') AND " +
			fmt.Sprintf(reachable, "oid"),
		"role, or one it may become, is a member of a predefined role that writes data, files or runs programs",
	},
	{
		// Ownership is a privilege that is never written down: the owner of a
		// relation may write it, and drop it, with an empty ACL.
		`SELECT count(*) > 0
		   FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		   LEFT JOIN LATERAL aclexplode(c.relacl) a ON true
		  WHERE c.relkind IN ('r', 'p', 'v', 'm', 'f', 'S')
		    AND ` + nonSystem + `
		    AND (` + fmt.Sprintf(reachable, "c.relowner") + `
		         OR (` + heldByReachable + `))`,
		"role, or one it may become, owns a table or holds a table privilege other than SELECT",
	},
	{
		// A column grant is its own ACL and appears in no table-level view:
		// UPDATE on one column is a write like any other.
		`SELECT count(*) > 0
		   FROM pg_attribute at
		   JOIN pg_class c ON c.oid = at.attrelid
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		   CROSS JOIN LATERAL aclexplode(at.attacl) a
		  WHERE ` + nonSystem + `
		    AND ` + heldByReachable,
		"role, or one it may become, holds a column privilege other than SELECT",
	},
	{
		"SELECT count(*) > 0 FROM pg_extension WHERE extname IN ('dblink', 'postgres_fdw', 'file_fdw')",
		"dblink or a foreign-data wrapper is installed: its functions reach other hosts from inside the database",
	},
}

// prove asks the probes in order and stops at the first that says the
// user can write.
func prove(ctx context.Context, q orm.DB) (dbq.Proof, error) {
	for _, p := range probes {
		var writable bool
		if _, err := q.QueryOneContext(ctx, pg.Scan(&writable), p.query); err != nil {
			return dbq.Proof{}, err
		}
		if writable {
			return dbq.Proof{Reason: p.reason}, nil
		}
	}
	return dbq.Proof{ReadOnly: true}, nil
}

// ProveReadOnly runs the probes on demand. A user that fails them never gets a
// connection, so the hook's refusal is the verdict — reachable and writable —
// not a fault of the network.
func (c *Client) ProveReadOnly(ctx context.Context) (dbq.Proof, error) {
	proof, err := prove(ctx, c.db)
	var refused *dbq.NotReadOnlyError
	if errors.As(err, &refused) {
		return dbq.Proof{Reason: refused.Reason}, nil
	}
	if err != nil {
		return dbq.Proof{}, classify(err)
	}
	return proof, nil
}

// Query runs one statement in a READ ONLY transaction, where the server refuses
// a write by itself — the text check above is a courtesy, this is the guarantee.
// The extended protocol takes exactly one command, so a second one smuggled past
// the text check is a parse error rather than a second transaction.
func (c *Client) Query(ctx context.Context, req dbq.Statement) (*dbq.Result, error) {
	qctx, cancel := context.WithCancel(ctx)
	defer cancel()
	rows := &rawRows{max: req.MaxRows, budget: req.MaxBytes, cancel: cancel}
	err := c.inTx(qctx, req.Timeout, req.Caller, func(tx *pg.Tx) error {
		// The statement belongs to the transaction, which deallocates it on
		// rollback; closing it here would take the connection with it.
		stmt, err := tx.Prepare(req.SQL)
		if err != nil {
			return err
		}
		if _, err := stmt.QueryContext(qctx, rows); err != nil {
			return err
		}
		return c.resolveTypes(qctx, tx, rows)
	})
	if rows.overflow {
		return nil, fmt.Errorf("%w: more than %d bytes read", dbq.ErrResultTooLarge, req.MaxBytes)
	}
	if err != nil {
		return nil, classify(err)
	}
	return rows.result(c.typeName), nil
}

// inTx runs fn on one connection inside a READ ONLY transaction with the timeout
// and the caller's name for pg_stat_activity, then rolls it back and releases
// the session's advisory locks. Rolled back, never committed: a SET the
// statement managed to run dies with the transaction instead of living on in the
// pool for the next caller.
func (c *Client) inTx(ctx context.Context, timeout time.Duration, caller string, fn func(tx *pg.Tx) error) error {
	cn := c.db.Conn()
	defer cn.Close()

	tx, err := cn.BeginContext(ctx)
	if err != nil {
		return err
	}
	err = func() error {
		if _, execErr := tx.ExecContext(ctx, "SET TRANSACTION READ ONLY"); execErr != nil {
			return execErr
		}
		if timeout > 0 {
			if _, execErr := tx.ExecContext(ctx, "SET LOCAL statement_timeout = ?", timeout.Milliseconds()); execErr != nil {
				return execErr
			}
		}
		if caller != "" {
			if _, execErr := tx.ExecContext(ctx, "SET LOCAL application_name = ?", caller); execErr != nil {
				return execErr
			}
		}
		return fn(tx)
	}()
	if rbErr := tx.RollbackContext(ctx); rbErr != nil && err == nil {
		return rbErr
	}
	if err == nil {
		_, err = cn.ExecContext(ctx, "SELECT pg_advisory_unlock_all()")
	}
	return err
}

// Close releases the pool.
func (c *Client) Close() error { return c.db.Close() }

// classify marks what the domain tells apart: statement_timeout (57014) and an
// unknown table (42P01) by their codes, socket and context timeouts by
// dbq.WrapTimeout.
func classify(err error) error {
	var pgErr pg.Error
	if errors.As(err, &pgErr) {
		switch pgErr.Field('C') {
		case sqlstateTimeout:
			return fmt.Errorf("%w: %s", dbq.ErrTimeout, pgErr.Field('M'))
		case sqlstateUnknownTable:
			return fmt.Errorf("%w: %s", dbq.ErrUnknownTable, pgErr.Field('M'))
		}
	}
	return dbq.WrapTimeout(err)
}
