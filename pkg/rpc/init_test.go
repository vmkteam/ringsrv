package rpc

import (
	"fmt"
	"strings"
	"testing"

	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/stretchr/testify/assert"
	"github.com/vmkteam/mcpkit/mcp"
)

// The instructions are the first and sometimes the only thing a model reads
// about this server. They drifted once already: after phase 2 they still
// promised a single tool and named error codes in a spelling the server had
// stopped using (E_TARGET_UNKNOWN against TargetUnknown). Both are the kind of
// lie that costs a call to discover.
func TestInstructions_DoNotLie(t *testing.T) {
	t.Parallel()
	// The instructions are the service own text now: mcpkit takes them as
	// InitDeps.Instructions, so the thing to test is the sheet, not the handshake
	// that carries it. With the budget on, so its paragraph is checked too.
	text := instructions("dev", true)

	assert.Contains(t, text, "dev", "the contour is the first thing to read")
	assert.NotContains(t, text, "E_", "error codes are CamelCase, not E_SCREAMING_SNAKE")

	// Every tool the instructions name has to be a tool this server has.
	for word := range strings.FieldsSeq(strings.NewReplacer(",", " ", ".", " ", "(", " ", ")", " ").Replace(text)) {
		if !strings.Contains(word, "_") || strings.HasPrefix(word, "ringsrv://") {
			continue
		}
		assert.Contains(t, target.KnownTools, word, "instructions name %q, which is not a tool", word)
	}

	// And every error code they name has to be one the server can return.
	known := []string{
		ErrCodeTargetUnknown, ErrCodeForbiddenRole, ErrCodeBadArgs, ErrCodeMethodNotAllowed,
		ErrCodePathNotAllowed, ErrCodeRPCMethodNotAllowed, ErrCodeQueryParamNotAllowed,
		ErrCodeUpstream, ErrCodeTimeout, ErrCodeJQFailed,
		ErrCodeIntentRequired, ErrCodeWriteBudget, ErrCodeRepoUnknown, ErrCodeRefUnknown,
		ErrCodePathDenied, ErrCodeNotIndexed, ErrCodeNoEngine, ErrCodeAmbiguous,
		ErrCodeRangeTooBig, ErrCodeNoPrev, ErrCodeSheetUnknown,
	}
	for _, code := range []string{"TargetUnknown", "PathNotAllowed", "JQFailed", "AmbiguousSymbol"} {
		assert.Contains(t, text, code, "the instructions promise %s", code)
		assert.Contains(t, known, code, "%s is promised but the server never returns it", code)
	}
}

// The budget is told about only where there is one: without it the field never
// comes, and a refusal by it cannot happen.
func TestInstructions_BudgetOnlyWhenOn(t *testing.T) {
	t.Parallel()
	off := instructions("dev", false)
	assert.NotContains(t, off, "budget")
	assert.NotContains(t, off, "бюджет")

	on := instructions("dev", true)
	assert.True(t, strings.HasPrefix(on, off), "the budget is added to the sheet, not woven into it")
	assert.Contains(t, on, "поле budget")
	assert.Contains(t, on, fmt.Sprintf("Отказ %d", mcp.CodeRateLimited), "the code the refusal really carries")
	assert.NotContains(t, on, "%!", "every verb filled")
}
