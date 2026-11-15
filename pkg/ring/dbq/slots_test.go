package dbq_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/vmkteam/ringsrv/pkg/ring/dbq"
	"github.com/vmkteam/ringsrv/pkg/ring/dbq/dbqtest"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// slotManager is a real-clock manager over one target with a pool of width.
func slotManager(t *testing.T, c *dbqtest.Client, width int, timeout time.Duration) *dbq.Manager {
	t.Helper()
	m := dbq.NewManager(dbq.Options{JQTimeout: time.Second, MaxBytes: 1 << 20})
	require.NoError(t, m.Add(dbq.Target{
		Name: "ch", Driver: target.DriverClickHouse, Client: c, MaxRows: 10,
		Timeout: timeout, Database: "events", MaxConcurrent: width,
	}))
	require.NoError(t, m.Prove(t.Context()))
	return m
}

// A query that queues for a connection reaches the server with its whole
// timeout: the clock starts when it has a connection, not when it asked for
// one. Queued inside the pool instead, the second query here would arrive with
// a third of its time spent on somebody else's.
func TestManager_WaitDoesNotSpendTimeout(t *testing.T) {
	t.Parallel()
	const hold, timeout = 400 * time.Millisecond, 200 * time.Millisecond
	entered := make(chan struct{})
	var once sync.Once
	c := dbqtest.New(nil)
	c.SetQuery(func(st dbq.Statement) (*dbq.Result, error) {
		first := false
		once.Do(func() { first = true })
		if first {
			close(entered)
			time.Sleep(hold)
		}
		return dbqtest.Rows(1)(st)
	})
	m := slotManager(t, c, 1, timeout)
	assert.Equal(t, 1, m.Width("ch"))
	assert.Equal(t, 1, m.Width("nosuch"), "an unknown target is one at a time")

	var wg sync.WaitGroup
	wg.Go(func() {
		_, ferr := m.Query(t.Context(), "ch", dbq.Request{SQL: "SELECT 1"})
		assert.Nil(t, ferr)
	})
	<-entered
	answer, ferr := m.Query(t.Context(), "ch", dbq.Request{SQL: "SELECT 2"})
	wg.Wait()
	require.Nil(t, ferr)
	assert.GreaterOrEqual(t, answer.Waited, hold-100*time.Millisecond, "the wait is reported")
	assert.Less(t, answer.Elapsed, hold, "and is not in the query's own time")

	deadlines := c.Deadlines()
	require.Len(t, deadlines, 2)
	assert.Greater(t, deadlines[1], timeout+time.Second-100*time.Millisecond,
		"the second query got its whole deadline after the wait")
}

// The wait is bounded by the caller alone, and a caller who leaves is told why.
func TestManager_WaitEndsWithTheCaller(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	c := dbqtest.New(nil)
	c.SetQuery(func(st dbq.Statement) (*dbq.Result, error) {
		once.Do(func() { close(entered); <-release })
		return dbqtest.Rows(1)(st)
	})
	m := slotManager(t, c, 1, time.Second)

	var wg sync.WaitGroup
	wg.Go(func() { _, _ = m.Query(t.Context(), "ch", dbq.Request{SQL: "SELECT 1"}) })
	<-entered
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, ferr := m.Query(ctx, "ch", dbq.Request{SQL: "SELECT 2"})
	close(release)
	wg.Wait()

	require.NotNil(t, ferr)
	assert.Equal(t, dbq.FailTimeout, ferr.Kind)
	assert.Contains(t, ferr.Error(), "waiting for a free connection")
	assert.GreaterOrEqual(t, ferr.Waited, 40*time.Millisecond)
}

// No more calls reach the client than the pool has connections, whoever makes
// them.
func TestManager_WidthCapsCalls(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	inflight, peak := 0, 0
	c := dbqtest.New(func(st dbq.Statement) (*dbq.Result, error) {
		mu.Lock()
		inflight++
		peak = max(peak, inflight)
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		inflight--
		mu.Unlock()
		return dbqtest.Rows(1)(st)
	})
	m := slotManager(t, c, 3, time.Second)

	var wg sync.WaitGroup
	for range 9 {
		wg.Go(func() {
			_, ferr := m.Query(t.Context(), "ch", dbq.Request{SQL: "SELECT 1"})
			assert.Nil(t, ferr)
		})
	}
	wg.Wait()
	assert.Equal(t, 3, peak)
}
