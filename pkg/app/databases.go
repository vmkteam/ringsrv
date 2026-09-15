package app

import (
	"context"
	"fmt"
	"time"

	"github.com/vmkteam/ringsrv/pkg/chdb"
	"github.com/vmkteam/ringsrv/pkg/db"
	"github.com/vmkteam/ringsrv/pkg/ring/dbq"
	"github.com/vmkteam/ringsrv/pkg/ring/target"
	"github.com/vmkteam/ringsrv/pkg/rpc"
)

// dbProveTimeout bounds the boot-time proof of every target at once: a database
// that does not answer in this long is unproven, not a reason to keep the
// listener closed. The re-proof that follows it runs on a cron — see cron.go.
const dbProveTimeout = 30 * time.Second

// initDatabases builds one client per catalogue database, picks the driver and
// runs the proof once at boot. A database that cannot be reached is logged and
// left unproven; one that answers and can write refuses the start.
func (a *App) initDatabases(ctx context.Context) error {
	if len(a.targets.Databases) == 0 {
		return nil
	}
	m := dbq.NewManager(dbq.Options{
		JQTimeout: a.cfg.Limits.JQTimeout,
		MaxBytes:  a.cfg.Limits.MaxBytes,
		Logger:    a.Logger,
		OnState:   rpc.RecordDBState,
	})
	for name, d := range a.targets.Databases {
		t, err := a.newDBTarget(name, d)
		if err != nil {
			return fmt.Errorf("database %q: %w", name, err)
		}
		if err := m.Add(t); err != nil {
			return err
		}
	}
	proveCtx, cancel := context.WithTimeout(ctx, dbProveTimeout)
	defer cancel()
	if err := m.Prove(proveCtx); err != nil {
		return err
	}
	a.db = m
	for _, name := range m.Names() {
		st, _ := m.State(name)
		d := a.targets.Databases[name]
		// tables is on the boot line: a target that proves read-only and sees
		// nothing looks identical to a healthy one everywhere else.
		a.Print(ctx, "database target", "target", name, "driver", d.Driver,
			"addr", d.Addr, "database", d.Database, "user", d.User, "proven", st.Proven,
			"tables", st.Tables, "reason", st.Reason)
	}
	return nil
}

// newDBTarget turns a catalogue entry into a domain target with its client.
// Limits the entry left at zero come from the instance config.
func (a *App) newDBTarget(name string, d *target.Database) (dbq.Target, error) {
	t := dbq.Target{
		Name: name, Driver: d.Driver, Password: d.Password,
		MaxRows: d.MaxRows, Timeout: d.Timeout, MaxBytes: d.MaxBytes,
		Redact: d.Redact, RedactRules: d.RedactRules,
		// The declared scope travels with the target: what a refusal names, and
		// what the probe compares the server's grants against.
		Database: d.Database, Schemas: d.Schemas,
	}
	if t.MaxRows <= 0 {
		t.MaxRows = a.cfg.Limits.DBMaxRows
	}
	if t.Timeout <= 0 {
		t.Timeout = a.cfg.Limits.DBTimeout
	}
	// A pool size the entry left at zero is the client's default.
	switch d.Driver {
	case target.DriverPostgres:
		t.Client = db.New(db.Options{
			Addr: d.Addr, Database: d.Database, User: d.User, Password: d.Password,
			PoolSize: d.MaxConcurrent, Timeout: t.Timeout, Schemas: d.Schemas, AppName: a.appName, Logger: a.Logger,
		})
	case target.DriverClickHouse:
		c, err := chdb.New(chdb.Options{
			Addr: d.Addr, Database: d.Database, User: d.User, Password: d.Password,
			MaxOpenConns: d.MaxConcurrent, Timeout: t.Timeout,
		})
		if err != nil {
			return dbq.Target{}, err
		}
		t.Client = c
	default:
		return dbq.Target{}, fmt.Errorf("unknown driver %q", d.Driver)
	}
	return t, nil
}

// closeDatabases releases the pools, after the cron, so a re-proof in flight is
// not cut off mid-query.
func (a *App) closeDatabases(ctx context.Context) {
	if a.db == nil {
		return
	}
	done := make(chan error, 1)
	go func() { done <- a.db.Close() }()
	select {
	case err := <-done:
		if err != nil {
			a.Error(ctx, "shutdown: closing database pools", "err", err.Error())
		}
	case <-ctx.Done():
		a.Error(context.WithoutCancel(ctx), "shutdown: database pools still closing, giving up on them")
	}
}
