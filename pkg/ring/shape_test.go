package ring

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmkteam/mcpkit/mcp"
	"github.com/vmkteam/mcpkit/redact"
)

// The argument may lower the cap and may raise it, but not past
// MaxAnswerBytes: the model's number is not the operator's.
func TestPickLimit(t *testing.T) {
	t.Parallel()
	const mib = 1 << 20
	cases := map[string]struct {
		call, target, instance, want int
	}{
		"no call: the target":                         {0, 4096, 32768, 4096},
		"no call, no target: the instance":            {0, 0, 32768, 32768},
		"nothing set anywhere":                        {0, 0, 0, 0},
		"the call narrows":                            {100, 4096, 32768, 100},
		"the call widens up to the quarter megabyte":  {100_000, 4096, 32768, 100_000},
		"past the quarter megabyte on a small target": {10 << 20, 4096, 32768, MaxAnswerBytes},
		"one byte past it":                            {MaxAnswerBytes + 1, 4096, 32768, MaxAnswerBytes},
		"nothing set, a call asks for a lot":          {10 << 20, 0, 0, MaxAnswerBytes},
		"a megabyte target, no call":                  {0, mib, 32768, mib},
		"a megabyte target, a call for half":          {500_000, mib, 32768, 500_000},
		"a megabyte target, a call for a quarter+":    {MaxAnswerBytes + 1, mib, 32768, MaxAnswerBytes + 1},
		"a megabyte target, a call past it":           {2 * mib, mib, 32768, mib},
		"a megabyte instance, a call past it":         {2 * mib, 0, mib, mib},
		"a megabyte instance, a target narrows it":    {500_000, 4096, mib, MaxAnswerBytes},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, PickLimit(tc.call, tc.target, tc.instance))
		})
	}
}

// One pipeline for an HTTP answer and a query result: jq, then the
// target's redaction over what is left, then the cut over the encoded text.
func TestShapeJSON(t *testing.T) {
	t.Parallel()
	body := []byte(`{"rows":[["ivan@example.com"],["x"]],"n":2}`)

	shaped, err := ShapeJSON(t.Context(), body, "", time.Second, redact.ModeOn, []string{redact.RuleEmail}, 0)
	require.NoError(t, err)
	assert.Equal(t, 1, shaped.Redacted.Hits[redact.RuleEmail])
	assert.False(t, shaped.AsText)
	assert.Len(t, shaped.Data.(map[string]any)["rows"], 2)

	shaped, err = ShapeJSON(t.Context(), body, ".rows | map(.[0])", time.Second, redact.ModeOff, nil, 0)
	require.NoError(t, err)
	assert.Equal(t, []any{"ivan@example.com", "x"}, shaped.Data, "the filter runs before the redaction")

	shaped, err = ShapeJSON(t.Context(), body, "", time.Second, redact.ModeOff, nil, 10)
	require.NoError(t, err)
	assert.True(t, shaped.AsText)
	assert.True(t, shaped.Truncated)
	assert.True(t, strings.HasSuffix(shaped.Text, mcp.TruncateMarker))
	assert.Equal(t, len(body), shaped.BytesTotal)

	_, err = ShapeJSON(t.Context(), body, ".rows.nope", time.Second, redact.ModeOff, nil, 0)
	require.ErrorIs(t, err, ErrJQRun)
	_, err = ShapeJSON(t.Context(), []byte("not json"), "", time.Second, redact.ModeOff, nil, 0)
	require.ErrorIs(t, err, ErrJQNotJSON)
}
