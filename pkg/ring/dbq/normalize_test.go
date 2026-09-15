package dbq

import (
	"encoding/json"
	"math"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stringer struct{ s string }

func (s stringer) String() string { return s.s }

// Every value form Normalize shapes: what a driver hands over and what the
// model reads.
func TestNormalize(t *testing.T) {
	t.Parallel()
	msk := time.FixedZone("MSK", 3*3600)
	at := time.Date(2026, 9, 2, 15, 4, 5, 0, msk)
	i32 := int32(7)
	var nilPtr *int32

	cases := map[string]struct{ in, want any }{
		"nil":            {nil, nil},
		"int":            {int64(42), int64(42)},
		"float":          {3.5, 3.5},
		"bool":           {true, true},
		"string":         {"x", "x"},
		"json number":    {json.Number("1.50"), json.Number("1.50")},
		"bytes":          {[]byte("hi"), "aGk="},
		"time in utc":    {at, "2026-09-02T12:04:05Z"},
		"time with nano": {at.Add(1500 * time.Microsecond), "2026-09-02T12:04:05.0015Z"},
		"big int":        {big.NewInt(1234567890123456789), "1234567890123456789"},
		"decimal-like":   {stringer{"12.340"}, "12.340"},
		"ip":             {net.ParseIP("10.0.0.1"), "10.0.0.1"},
		"pointer":        {&i32, int32(7)},
		"nil pointer":    {nilPtr, nil},
		"typed slice":    {[]int32{1, 2}, []any{int32(1), int32(2)}},
		"nil slice":      {[]string(nil), nil},
		"nested":         {[]any{[]byte("a"), at}, []any{"YQ==", "2026-09-02T12:04:05Z"}},
		"map":            {map[string]int{"a": 1}, map[string]any{"a": 1}},
		"raw json":       {json.RawMessage(`{"k":[1]}`), map[string]any{"k": []any{float64(1)}}},
		"nan":            {math.NaN(), "NaN"},
		"inf":            {math.Inf(1), "Infinity"},
		"neg inf":        {float32(math.Inf(-1)), "-Infinity"},
		"typed nil big":  {(*big.Int)(nil), nil},
		"max uint64":     {uint64(math.MaxUint64), uint64(math.MaxUint64)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := Normalize(tc.in)
			assert.Equal(t, tc.want, got)
			_, err := json.Marshal(got)
			require.NoError(t, err, "whatever comes out has to encode")
		})
	}
}
