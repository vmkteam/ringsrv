package code

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A reformatting commit is recognised by a narrow set of words. The negative
// half is the point: "fmt", "format" and "rename" on their own used to send
// the answer to the commit before the one that wrote the behaviour.
func TestReformatSubjects(t *testing.T) {
	t.Parallel()
	for _, s := range []string{
		"gofmt", "go fmt ./...", "goimports", "reformat imports", "reformatting",
		"formatting only", "fix lint", "linter fixes", "golangci-lint",
		"whitespace", "fix typo", "rename variable", "rename file",
	} {
		assert.True(t, reformatRe.MatchString(s), s)
	}
	for _, s := range []string{
		"PLF-42 add order creation",
		"fix panic in ProducerImageIDs",
		"wrap with fmt.Errorf",
		"rename column in migration",
		"format prices",
		"add formatter for prices",
		"rename endpoint",
	} {
		assert.False(t, reformatRe.MatchString(s), s)
	}
}

// A commit with an issue key is the answer whatever its subject says: it was
// done for that task. Only a keyless formatting commit hides the real one.
func TestReformatHides(t *testing.T) {
	t.Parallel()
	assert.True(t, reformatHides("gofmt and lint fixes", ""))
	assert.False(t, reformatHides("PLF-1 fix lint", "PLF-1"), "the key wins over the wording")
	assert.False(t, reformatHides("add order creation", ""), "not a reformat at all")
}
