package ring

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const promBody = `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"job":"apisrv"},"value":[1,"3"]}]}}`

func TestApplyJQ(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("empty expression returns the parsed body", func(t *testing.T) {
		t.Parallel()
		got, err := ApplyJQ(ctx, "", []byte(promBody), time.Second)
		require.NoError(t, err)
		m, ok := got.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "success", m["status"])
	})

	t.Run("identity is the same path", func(t *testing.T) {
		t.Parallel()
		got, err := ApplyJQ(ctx, JQIdentity, []byte(promBody), time.Second)
		require.NoError(t, err)
		assert.IsType(t, map[string]any{}, got)
	})

	// This is the DefaultJQ of the prom profile: the envelope is what makes
	// most of the answer, and dropping it is the whole point of D3.
	t.Run("DefaultJQ-style filter unwraps the envelope", func(t *testing.T) {
		t.Parallel()
		got, err := ApplyJQ(ctx, ".data.result", []byte(promBody), time.Second)
		require.NoError(t, err)
		arr, ok := got.([]any)
		require.True(t, ok)
		assert.Len(t, arr, 1)
	})

	// jq programs routinely emit a stream; a tool answer has to be one value.
	t.Run("multiple outputs become an array", func(t *testing.T) {
		t.Parallel()
		got, err := ApplyJQ(ctx, ".[]", []byte(`[1,2,3]`), time.Second)
		require.NoError(t, err)
		assert.Equal(t, []any{1, 2, 3}, got, "integers stay integers: an id past 2^53 must not come back as a float")
	})

	// An expression that selects nothing is an answer — "no such series" — and
	// must not be reported as a failure.
	t.Run("no output is not an error", func(t *testing.T) {
		t.Parallel()
		got, err := ApplyJQ(ctx, `.[] | select(.x > 10)`, []byte(`[{"x":1}]`), time.Second)
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("bad expression", func(t *testing.T) {
		t.Parallel()
		_, err := ApplyJQ(ctx, ".data[", []byte(promBody), time.Second)
		require.ErrorIs(t, err, ErrJQCompile)
	})

	t.Run("expression fails on this input", func(t *testing.T) {
		t.Parallel()
		_, err := ApplyJQ(ctx, ".data | keys[0] + 1", []byte(promBody), time.Second)
		require.ErrorIs(t, err, ErrJQRun)
	})

	t.Run("non-json body", func(t *testing.T) {
		t.Parallel()
		_, err := ApplyJQ(ctx, ".", []byte("<html>502 Bad Gateway</html>"), time.Second)
		require.ErrorIs(t, err, ErrJQNotJSON)
	})
}

// gojq has no step limit of its own: without the timeout one pathological
// expression holds a request forever.
func TestApplyJQ_Timeout(t *testing.T) {
	t.Parallel()
	start := time.Now()
	_, err := ApplyJQ(context.Background(), "def f: f; f", []byte(`{}`), 100*time.Millisecond)
	require.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second, "the expression must be cut off, not waited out")
}

func TestTopLevelKeys(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []string{"data", "status"}, TopLevelKeys([]byte(promBody)))
	assert.Nil(t, TopLevelKeys([]byte(`[1,2]`)), "an array has no keys to suggest")
	assert.Nil(t, TopLevelKeys([]byte("not json")))
}

// gojq has no memory limit. The two cheap ways to ask for a gigabyte — string
// repetition and range — are refused or capped before anything runs; the
// internals the prelude wraps are not callable from an expression.
func TestApplyJQ_MemoryGuard(t *testing.T) {
	t.Parallel()
	body := []byte(`{"n": 3, "s": "x", "ms": 1500}`)

	refused := map[string]string{
		`"a" * 2000000000`:        "string repetition",
		`("a") * 3`:               "string repetition",
		`.s * 2000000000`:         "multiplier",
		`_range(0; 100000000; 1)`: "internal function",
		`[range(100000000)]`:      "range: at most",
		`[range(0; 100000000)]`:   "range: at most",
		`[range(0; 1; 0)]`:        "range: at most",
	}
	for expr, want := range refused {
		t.Run(expr, func(t *testing.T) {
			t.Parallel()
			_, err := ApplyJQ(context.Background(), expr, body, 2*time.Second)
			require.Error(t, err)
			assert.Contains(t, err.Error(), want)
		})
	}

	// Ordinary arithmetic and a small range keep working: the guard is for
	// size, not for multiplication.
	allowed := map[string]any{
		`.ms * 1000`:             1_500_000,
		`.n * .n`:                9,
		`[range(3)]`:             []any{0, 1, 2},
		`[range(1; 3)]`:          []any{1, 2},
		`{a: .s} * {b: .n} | .b`: 3,
	}
	for expr, want := range allowed {
		t.Run(expr, func(t *testing.T) {
			t.Parallel()
			got, err := ApplyJQ(context.Background(), expr, body, 2*time.Second)
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
}

// An integer past 2^53 — a bigint id, a UInt64 — comes through the filter
// and back out exactly; decoded into a float64 it would change by a digit.
func TestApplyJQ_ExactIntegers(t *testing.T) {
	t.Parallel()
	body := []byte(`{"ids":[9007199254740993,18446744073709551615,1.5,-7]}`)
	got, err := ApplyJQ(t.Context(), ".ids", body, time.Second)
	require.NoError(t, err)
	out, err := json.Marshal(got)
	require.NoError(t, err)
	assert.Equal(t, `[9007199254740993,18446744073709551615,1.5,-7]`, string(out))

	got, err = ApplyJQ(t.Context(), "", body, time.Second)
	require.NoError(t, err)
	out, err = json.Marshal(got)
	require.NoError(t, err)
	assert.Contains(t, string(out), "18446744073709551615", "the identity path keeps them too")

	_, err = ApplyJQ(t.Context(), "", []byte(`{"a":1} {"b":2}`), time.Second)
	require.ErrorIs(t, err, ErrJQNotJSON, "two documents are not one")
}
