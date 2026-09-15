package gitlab

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// GitLab sends the project id under two names and which one arrives depends on
// its version, so both are read. Everything else about the payload is ignored
// on purpose: this service needs to know that something was pushed and where.
func TestParsePush(t *testing.T) {
	t.Parallel()

	e, err := ParsePush([]byte(`{"object_kind":"push","project":{"id":42},"commits":[{"id":"abc"}]}`))
	require.NoError(t, err)
	assert.True(t, e.IsPush())
	assert.Equal(t, 42, e.ProjectID)

	e, err = ParsePush([]byte(`{"object_kind":"push","project_id":42}`))
	require.NoError(t, err)
	assert.True(t, e.IsPush(), "the older field name still names a project")
	assert.Equal(t, 42, e.ProjectID)

	// A tag push is a valid event about something we do not act on.
	e, err = ParsePush([]byte(`{"object_kind":"tag_push","project":{"id":42}}`))
	require.NoError(t, err)
	assert.False(t, e.IsPush())

	// A push with no project names nothing to fetch.
	e, err = ParsePush([]byte(`{"object_kind":"push"}`))
	require.NoError(t, err)
	assert.False(t, e.IsPush())

	_, err = ParsePush([]byte(`not json`))
	require.Error(t, err)
}
