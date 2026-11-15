package dbq_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vmkteam/ringsrv/pkg/ring/dbq"
	"github.com/vmkteam/ringsrv/pkg/ring/dbq/dbqtest"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const mib = 1 << 20

// wideRows answers n rows of one 1000-byte string: about n kilobytes of JSON.
func wideRows(n int) func(dbq.Statement) (*dbq.Result, error) {
	return func(req dbq.Statement) (*dbq.Result, error) {
		res := &dbq.Result{Columns: []dbq.Column{{Name: "blob", Type: "String"}}, Rows: [][]any{}}
		for range min(n, req.MaxRows) {
			res.Rows = append(res.Rows, []any{strings.Repeat("x", 1000)})
		}
		return res, nil
	}
}

func limitManager(t *testing.T, c *dbqtest.Client, targetBytes, instanceBytes int) *dbq.Manager {
	t.Helper()
	m := dbq.NewManager(dbq.Options{JQTimeout: time.Second, MaxBytes: instanceBytes})
	require.NoError(t, m.Add(dbq.Target{
		Name: "ch", Driver: target.DriverClickHouse, Client: c, MaxRows: 5000,
		Timeout: time.Second, Database: "events", MaxBytes: targetBytes,
	}))
	require.NoError(t, m.Prove(t.Context()))
	return m
}

// A target allowed a megabyte answers a call asking for half of one with half
// of one — not with the quarter megabyte a small target caps the call at.
func TestManager_CallWidensToTheTargetLimit(t *testing.T) {
	t.Parallel()
	m := limitManager(t, dbqtest.New(wideRows(400)), mib, 32768)

	answer, ferr := m.Query(t.Context(), "ch", dbq.Request{SQL: "SELECT blob FROM t", MaxBytes: 500_000})
	require.Nil(t, ferr)
	assert.False(t, answer.Truncated, "400 KB fits the 500 KB the call asked for")
	assert.Greater(t, answer.BytesTotal, 400_000)

	small := limitManager(t, dbqtest.New(wideRows(400)), 32768, 32768)
	answer, ferr = small.Query(t.Context(), "ch", dbq.Request{SQL: "SELECT blob FROM t", MaxBytes: 500_000})
	require.Nil(t, ferr)
	assert.True(t, answer.Truncated, "a small target caps the call at the quarter megabyte, as before")
}

// The wire budget follows the answer the statement may give: twice its limit,
// never less than the megabyte every target had.
func TestManager_ReadBudgetFollowsTheAnswer(t *testing.T) {
	t.Parallel()
	var (
		mu  sync.Mutex
		got []int
	)
	record := func(st dbq.Statement) (*dbq.Result, error) {
		mu.Lock()
		got = append(got, st.MaxBytes)
		mu.Unlock()
		return dbqtest.Rows(1)(st)
	}
	cases := []struct {
		name                        string
		targetBytes, instance, call int
		want                        int
	}{
		{"the instance default", 0, 32768, 0, 4 * 256 << 10},
		{"a megabyte target", mib, 32768, 0, 2 * mib},
		{"a megabyte target, a call narrows it", mib, 32768, 100_000, 4 * 256 << 10},
		{"a megabyte target, a call for 900 KB", mib, 32768, 900_000, 1_800_000},
		{"a small target, a call past the ceiling", 32768, 32768, 10 * mib, 4 * 256 << 10},
	}
	for _, tc := range cases {
		got = nil
		m := limitManager(t, dbqtest.New(record), tc.targetBytes, tc.instance)
		_, ferr := m.Query(t.Context(), "ch", dbq.Request{SQL: "SELECT 1", MaxBytes: tc.call})
		require.Nil(t, ferr, tc.name)
		require.Len(t, got, 1, tc.name)
		assert.Equal(t, tc.want, got[0], tc.name)
	}
}

// A wide answer that fits a megabyte limit comes back whole, instead of failing
// on a wire budget of a megabyte that its raw values overran first.
func TestManager_WideAnswerFitsItsLimit(t *testing.T) {
	t.Parallel()
	// 1.05 MB of values: over the old fixed wire budget of 1 MiB, under the
	// answer limit of 1.2 MB.
	readOver := func(st dbq.Statement) (*dbq.Result, error) {
		res, err := wideRows(1050)(st)
		if err != nil {
			return nil, err
		}
		if raw := len(res.Rows) * 1000; raw > st.MaxBytes {
			return nil, fmt.Errorf("%w: more than %d bytes read", dbq.ErrResultTooLarge, st.MaxBytes)
		}
		return res, nil
	}
	m := limitManager(t, dbqtest.New(readOver), 1_200_000, 32768)
	answer, ferr := m.Query(t.Context(), "ch", dbq.Request{SQL: "SELECT blob FROM t"})
	require.Nil(t, ferr, "the wire budget is twice the answer limit")
	assert.False(t, answer.Truncated)
	assert.Equal(t, 1050, answer.RowsReturned)
}
