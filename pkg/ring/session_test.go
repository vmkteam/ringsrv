package ring

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessions_TraceIsStablePerCaller(t *testing.T) {
	t.Parallel()
	m := NewSessions(time.Hour, nil)

	first := m.Get("alice").TraceID
	assert.Equal(t, first, m.Get("alice").TraceID, "one investigation, one trace")
	assert.NotEqual(t, first, m.Get("bob").TraceID)
	assert.True(t, strings.HasPrefix(first, TracePrefix), "a report says where to look: %s", first)
}

// The window is inactivity: an investigation has gaps, but yesterday's budget
// must not carry into today.
func TestSessions_ExpiresAfterTTL(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	m := NewSessions(time.Hour, func() time.Time { return now })

	first := m.Get("alice").TraceID
	m.AddWrite("alice")

	now = now.Add(30 * time.Minute)
	assert.Equal(t, first, m.Get("alice").TraceID, "still the same session")
	assert.Equal(t, 2, m.AddWrite("alice"))

	now = now.Add(2 * time.Hour)
	second := m.Get("alice")
	assert.NotEqual(t, first, second.TraceID, "a cold session is a new one")
	assert.Zero(t, second.Writes, "and its budget starts over")
}

func TestSessions_WritesAreCountedPerCaller(t *testing.T) {
	t.Parallel()
	m := NewSessions(time.Hour, nil)

	require.Equal(t, 1, m.AddWrite("alice"))
	require.Equal(t, 2, m.AddWrite("alice"))
	assert.Equal(t, 1, m.AddWrite("bob"), "one caller cannot spend another's budget")
	assert.Equal(t, 2, m.Len())
}

func TestSessions_SweepsExpired(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	m := NewSessions(time.Minute, func() time.Time { return now })

	m.Get("alice")
	now = now.Add(time.Hour)
	m.Get("bob")
	assert.Equal(t, 1, m.Len(), "cold entries do not pile up")
}
