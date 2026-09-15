package dbq

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"time"
)

// Normalize turns a driver value into what travels in JSON: numbers as
// numbers, decimals as strings so no digit is lost, time as RFC 3339 in UTC,
// bytes as base64, nil as null, and arrays and maps recursively. A pointer —
// a Nullable column in ClickHouse — is
// followed; anything with a String method and no better shape (a decimal,
// a UUID, an IP) is its string.
func Normalize(v any) any {
	// A typed nil — a Nullable column the driver hands over as a nil
	// pointer of its type — is null before it is anything else.
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Pointer && rv.IsNil() {
		return nil
	}
	switch t := v.(type) {
	case nil:
		return nil
	case float64:
		return finite(t)
	case float32:
		return finite(float64(t))
	case bool, string, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, json.Number:
		return t
	case []byte:
		return base64.StdEncoding.EncodeToString(t)
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano)
	case *big.Int, *big.Float, *big.Rat:
		return fmt.Sprint(t)
	case json.RawMessage:
		var out any
		if err := json.Unmarshal(t, &out); err == nil {
			return out
		}
		return string(t)
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = Normalize(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, item := range t {
			out[k] = Normalize(item)
		}
		return out
	}

	return normalizeReflect(v)
}

// finite keeps a float as a number when JSON can carry it and names it
// otherwise: NaN and the infinities have no JSON form, and one of them in
// a cell must not cost the whole answer.
func finite(f float64) any {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	default:
		return f
	}
}

// normalizeReflect handles what the type switch could not name: a pointer
// or interface is followed, a named type with a String method — a decimal,
// a UUID, an IP — is its string whatever it is made of underneath, and a
// typed slice or map is walked.
func normalizeReflect(v any) any {
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return nil
		}
		if s, ok := v.(fmt.Stringer); ok {
			return s.String()
		}
		return Normalize(rv.Elem().Interface())
	}
	if s, ok := v.(fmt.Stringer); ok {
		return s.String()
	}
	if rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			return nil
		}
		out := make([]any, rv.Len())
		for i := range rv.Len() {
			out[i] = Normalize(rv.Index(i).Interface())
		}
		return out
	}
	if rv.Kind() == reflect.Map {
		out := make(map[string]any, rv.Len())
		for _, k := range rv.MapKeys() {
			out[fmt.Sprint(k.Interface())] = Normalize(rv.MapIndex(k).Interface())
		}
		return out
	}
	return v
}
