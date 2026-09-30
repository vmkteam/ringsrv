package tracker

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArtefacts(t *testing.T) {
	t.Parallel()
	assert.Equal(t,
		[]string{"docs/llm/research/01-x.md"},
		artefacts("research: docs/llm/research/01-x.md"))
	assert.Equal(t,
		[]string{"https://git.example.com/x/-/blob/main/docs/llm/spec/01.md"},
		artefacts("спека https://git.example.com/x/-/blob/main/docs/llm/spec/01.md"))
	assert.Empty(t, artefacts("обсудили, решили не делать"))
}

// Trackers disagree about names. YouTrack answers summary/description with
// comments[].text; Jira wraps the same things in fields and calls a comment
// body. why has to read both, or a repository on Jira passes validation and
// then silently returns no enrichment forever.
func TestParseIssue(t *testing.T) {
	t.Parallel()

	t.Run("youtrack", func(t *testing.T) {
		t.Parallel()
		issue := Parse([]byte(`{
			"summary": "Создание заказа",
			"description": "идемпотентно",
			"comments": [{"text": "research: docs/llm/research/03-orders.md"}]
		}`), "ABC-42")
		require.NotNil(t, issue)
		assert.Equal(t, "Создание заказа", issue.Summary)
		assert.Equal(t, "идемпотентно", issue.Description)
		assert.Equal(t, []string{"docs/llm/research/03-orders.md"}, issue.Artifacts)
	})

	t.Run("jira", func(t *testing.T) {
		t.Parallel()
		issue := Parse([]byte(`{
			"key": "ABC-42",
			"fields": {
				"summary": "Order creation",
				"description": "idempotent",
				"comment": {"comments": [{"body": "spec: docs/llm/spec/02-orders.md"}]}
			}
		}`), "ABC-42")
		require.NotNil(t, issue)
		assert.Equal(t, "Order creation", issue.Summary)
		assert.Equal(t, []string{"docs/llm/spec/02-orders.md"}, issue.Artifacts)
	})

	// An issue with nothing useful in it is still an answer: the key was
	// resolved, the tracker replied, there is simply no summary.
	t.Run("empty issue", func(t *testing.T) {
		t.Parallel()
		issue := Parse([]byte(`{"$type":"Issue"}`), "ABC-42")
		require.NotNil(t, issue)
		assert.Equal(t, "ABC-42", issue.ID)
		assert.Empty(t, issue.Summary)
	})

	t.Run("not json at all", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, Parse([]byte(`<html>gateway timeout</html>`), "ABC-42"))
	})
}
