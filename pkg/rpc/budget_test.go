package rpc

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The budget spares what costs nothing and what a caller needs to learn to
// spend less; every tool that reaches an upstream still pays.
func TestExemptFromBudget(t *testing.T) {
	t.Parallel()
	spared := [][2]string{
		{"tools/call", ToolHelp},
		{"tools/call", ToolRepoMap},
		{"resources/read", "ringsrv://tools/db.md"},
		{"prompts/get", "triage"},
		{"initialize", ""},
		{"ping", ""},
		{"server/discover", ""},
		{"tools/list", ""},
		{"resources/list", ""},
		{"resources/templates/list", ""},
		{"prompts/list", ""},
		{"notifications/initialized", ""},
	}
	for _, c := range spared {
		assert.Truef(t, ExemptFromBudget(c[0], c[1]), "%s %s", c[0], c[1])
	}

	paid := [][2]string{
		{"tools/call", ToolAPICall},
		{"tools/call", ToolDBQuery},
		{"tools/call", ToolCodeSearch},
		{"tools/call", ToolCodeRead},
		{"tools/call", ""},
		{"tools/call", "Help"},
		{"", ""},
	}
	for _, c := range paid {
		assert.Falsef(t, ExemptFromBudget(c[0], c[1]), "%s %s", c[0], c[1])
	}
}
