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
	assert.Equal(t, 100, PickLimit(100, 4096, 32768))
	assert.Equal(t, 4096, PickLimit(0, 4096, 32768))
	assert.Equal(t, 32768, PickLimit(0, 0, 32768))
	assert.Equal(t, MaxAnswerBytes, PickLimit(10<<20, 4096, 32768))
	assert.Equal(t, MaxAnswerBytes, PickLimit(MaxAnswerBytes+1, 4096, 32768))
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
