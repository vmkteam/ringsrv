package rpc

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The outcome label drives the dashboards and at least one alert. Mapping
// every failure to "forbidden" — which is what the code tools did — makes a
// git timeout look like a permissions problem at three in the morning.
func TestOutcomeOf(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		ErrCodeForbiddenRole:        outcomeForbidden,
		ErrCodePathDenied:           outcomeForbidden,
		ErrCodeIntentRequired:       outcomeForbidden,
		ErrCodeMethodNotAllowed:     outcomeForbidden,
		ErrCodePathNotAllowed:       outcomeForbidden,
		ErrCodeHeaderNotAllowed:     outcomeForbidden,
		ErrCodeRPCMethodNotAllowed:  outcomeForbidden,
		ErrCodeQueryParamNotAllowed: outcomeForbidden,
		ErrCodeBadArgs:              outcomeBadArgs,
		ErrCodeRefUnknown:           outcomeBadArgs,
		ErrCodeAmbiguous:            outcomeBadArgs,
		ErrCodeRangeTooBig:          outcomeBadArgs,
		ErrCodeTimeout:              outcomeTimeout,
		ErrCodeUpstream:             outcomeError,
		ErrCodeNoEngine:             outcomeError,
		"SomethingNewNobodyMapped":  outcomeError,
	}
	for code, want := range cases {
		t.Run(code, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, want, outcomeOf(code))
		})
	}
}

// Every tool this server dispatches has to be in the list initSeries walks,
// or its metric shows "no data" while it fails.
func TestAllToolsCovered(t *testing.T) {
	t.Parallel()
	for _, tool := range []string{
		ToolAPICall, ToolRepoMap, ToolCodeRead, ToolCodeSearch,
		ToolCodeHistory, ToolCodeRefs, ToolBlastRadius, ToolWhy,
	} {
		assert.Contains(t, allTools, tool)
	}
}
