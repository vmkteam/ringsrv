package ring

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/itchyny/gojq"
)

// Errors are distinguished because they need different answers: a broken
// expression is the caller's to fix, a non-JSON body is nobody's fault.
var (
	// ErrJQCompile means the expression itself is wrong.
	ErrJQCompile = errors.New("jq: bad expression")
	// ErrJQRun means the expression is valid but failed on this input.
	ErrJQRun = errors.New("jq: failed")
	// ErrJQNotJSON means the body could not be parsed at all, so there is
	// nothing to filter.
	ErrJQNotJSON = errors.New("jq: body is not json")
)

// JQIdentity is the expression that changes nothing.
const JQIdentity = "."

// ApplyJQ runs expr over a JSON body. An empty expression or "." parses the body
// and returns it as is, keeping one code path for filtered and unfiltered
// answers. Multiple outputs are wrapped into an array: jq programs routinely
// emit a stream, and a tool answer has to be one value.
func ApplyJQ(ctx context.Context, expr string, body []byte, timeout time.Duration) (any, error) {
	input, err := decodeJSON(body)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrJQNotJSON, err)
	}
	if expr == "" || expr == JQIdentity {
		return input, nil
	}

	code, err := compileJQ(expr)
	if err != nil {
		return nil, err
	}

	// The only thing between a pathological expression and a stuck request:
	// gojq has no step limit of its own.
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	var out []any
	iter := code.RunWithContext(ctx, input)
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if err, isErr := v.(error); isErr {
			var halt *gojq.HaltError
			if errors.As(err, &halt) && halt.Value() == nil {
				break // `halt` without a value is a normal early exit
			}
			return nil, fmt.Errorf("%w: %w", ErrJQRun, err)
		}
		out = append(out, v)
	}

	switch len(out) {
	case 0:
		// An expression that selects nothing is an answer — "no such series" —
		// not a failure.
		return nil, nil
	case 1:
		return out[0], nil
	default:
		return out, nil
	}
}

// decodeJSON parses a body with integers kept exact. Decoding straight into any
// turns every number into a float64, and an id past 2^53 comes back silently
// changed by a digit or two. Numbers are read as text and turned into int,
// *big.Int or float64 — the forms gojq takes and encoding/json writes verbatim.
func decodeJSON(body []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after the JSON value")
	}
	return exactNumbers(v), nil
}

func exactNumbers(v any) any {
	switch t := v.(type) {
	case json.Number:
		if i, err := strconv.ParseInt(t.String(), 10, 64); err == nil {
			return int(i)
		}
		if b, ok := new(big.Int).SetString(t.String(), 10); ok {
			return b
		}
		f, _ := t.Float64()
		return f
	case []any:
		for i, item := range t {
			t[i] = exactNumbers(item)
		}
	case map[string]any:
		for k, item := range t {
			t[k] = exactNumbers(item)
		}
	}
	return v
}

// CompileJQ reports whether expr would be accepted by ApplyJQ. The catalogue
// validator runs it over every DefaultJQ, so a typo fails the start-up instead
// of every call that trusted the default.
func CompileJQ(expr string) error {
	if expr == "" || expr == JQIdentity {
		return nil
	}
	_, err := compileJQ(expr)
	return err
}

// compileJQ parses expr twice: bare, so the guard walks what the caller wrote,
// and behind the prelude, since a definition only applies to what follows it.
func compileJQ(expr string) (*gojq.Code, error) {
	query, err := gojq.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrJQCompile, err)
	}
	if gerr := guardJQ(query); gerr != nil {
		return nil, fmt.Errorf("%w: %w", ErrJQCompile, gerr)
	}
	query, err = gojq.Parse(jqPrelude + expr)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrJQCompile, err)
	}
	code, err := gojq.Compile(query)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrJQCompile, err)
	}
	return code, nil
}

// TopLevelKeys lists the keys of a JSON object body, sorted. It travels with a
// JQFailed error so the expression can be fixed rather than guessed: usually the
// model filtered `.result` where the body has `.data.result`.
func TopLevelKeys(body []byte) []string {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// What follows bounds the memory a jq expression can ask for: gojq has no memory
// limit and the timeout only caps time, so `"a" * 2000000000` allocates two
// gigabytes and `[range(100000000)]` takes a 512 MB task down within two
// seconds. The engine offers no hook for its operators, so the guard is a
// prelude that shadows the unbounded builtins plus a walk that refuses what the
// prelude cannot reach.
//
// A guard, not a proof: a string from the response times a number from the
// response gets through, and the restart policy is what stands behind that.

// JQMaxRange caps how many values range may produce.
const JQMaxRange = 1_000_000

// JQMaxMultiplier caps a numeric literal next to "*": a filter over an API
// response converts units, it does not multiply by millions.
const JQMaxMultiplier = 1_000_000

// jqPrelude redefines range with a cap: user definitions win over builtin.jq in
// gojq's lookup, which is the one place a builtin can be overridden.
var jqPrelude = fmt.Sprintf(
	`def range($e): if ($e | type) != "number" or $e > %[1]d then error("range: at most %[1]d values") else _range(0; $e; 1) end; `+
		`def range($s; $e): if (($e - $s) | type) != "number" or $e - $s > %[1]d then error("range: at most %[1]d values") else _range($s; $e; 1) end; `+
		`def range($s; $e; $st): if $st == 0 or ((($e - $s) / $st) | type) != "number" or ($e - $s) / $st > %[1]d then error("range: at most %[1]d values") else _range($s; $e; $st) end; `,
	JQMaxRange)

// guardJQ walks the parsed expression and refuses what the prelude cannot
// bound: calls to internal functions (they start with "_", and "_range" is the
// uncapped one the prelude wraps), string repetition, and multiplication by a
// literal large enough to be about size rather than units.
func guardJQ(q *gojq.Query) error {
	if q == nil {
		return nil
	}
	for _, fd := range q.FuncDefs {
		if err := guardJQ(fd.Body); err != nil {
			return err
		}
	}
	if q.Op == gojq.OpMul {
		if isStringLiteral(q.Left) || isStringLiteral(q.Right) {
			return errors.New("jq: string repetition (\"s\" * n) is not allowed")
		}
		if n, ok := numberLiteral(q.Left); ok && n > JQMaxMultiplier {
			return fmt.Errorf("jq: multiplier %v is too large", n)
		}
		if n, ok := numberLiteral(q.Right); ok && n > JQMaxMultiplier {
			return fmt.Errorf("jq: multiplier %v is too large", n)
		}
	}
	if err := guardJQ(q.Left); err != nil {
		return err
	}
	if err := guardJQ(q.Right); err != nil {
		return err
	}
	return guardTerm(q.Term)
}

func guardTerm(t *gojq.Term) error {
	if t == nil {
		return nil
	}
	if t.Func != nil {
		if strings.HasPrefix(t.Func.Name, "_") {
			return fmt.Errorf("jq: internal function %s is not allowed", t.Func.Name)
		}
		for _, a := range t.Func.Args {
			if err := guardJQ(a); err != nil {
				return err
			}
		}
	}
	for _, q := range termQueries(t) {
		if err := guardJQ(q); err != nil {
			return err
		}
	}
	if t.Unary != nil {
		return guardTerm(t.Unary.Term)
	}
	return nil
}

// termQueries lists every sub-query a term can carry, so the walk misses no
// place an expression can hide.
func termQueries(t *gojq.Term) []*gojq.Query {
	var qs []*gojq.Query
	add := func(q *gojq.Query) {
		if q != nil {
			qs = append(qs, q)
		}
	}
	addIndex := func(i *gojq.Index) {
		if i == nil {
			return
		}
		add(i.Start)
		add(i.End)
		if i.Str != nil {
			qs = append(qs, i.Str.Queries...)
		}
	}
	add(t.Query)
	addIndex(t.Index)
	if t.Str != nil {
		qs = append(qs, t.Str.Queries...)
	}
	if t.Object != nil {
		for _, kv := range t.Object.KeyVals {
			add(kv.KeyQuery)
			add(kv.Val)
			if kv.KeyString != nil {
				qs = append(qs, kv.KeyString.Queries...)
			}
		}
	}
	if t.Array != nil {
		add(t.Array.Query)
	}
	if t.If != nil {
		add(t.If.Cond)
		add(t.If.Then)
		add(t.If.Else)
		for _, e := range t.If.Elif {
			add(e.Cond)
			add(e.Then)
		}
	}
	if t.Try != nil {
		add(t.Try.Body)
		add(t.Try.Catch)
	}
	if t.Reduce != nil {
		add(t.Reduce.Query)
		add(t.Reduce.Start)
		add(t.Reduce.Update)
	}
	if t.Foreach != nil {
		add(t.Foreach.Query)
		add(t.Foreach.Start)
		add(t.Foreach.Update)
		add(t.Foreach.Extract)
	}
	if t.Label != nil {
		add(t.Label.Body)
	}
	for _, s := range t.SuffixList {
		addIndex(s.Index)
	}
	return qs
}

// isStringLiteral reports whether a query is a bare string literal, looking
// through parentheses.
func isStringLiteral(q *gojq.Query) bool {
	t := bareTerm(q)
	return t != nil && t.Type == gojq.TermTypeString
}

// numberLiteral returns the value of a bare numeric literal, looking through
// parentheses and a leading minus.
func numberLiteral(q *gojq.Query) (float64, bool) {
	t := bareTerm(q)
	if t == nil {
		return 0, false
	}
	if t.Type == gojq.TermTypeUnary && t.Unary != nil {
		t = t.Unary.Term
	}
	if t == nil || t.Type != gojq.TermTypeNumber {
		return 0, false
	}
	n, err := strconv.ParseFloat(t.Number, 64)
	if err != nil {
		return 0, false
	}
	return math.Abs(n), true
}

// bareTerm returns the single term of a query without operators or suffixes,
// unwrapping "( ... )" as many times as needed.
func bareTerm(q *gojq.Query) *gojq.Term {
	for q != nil && q.Term != nil && q.Left == nil && q.Right == nil && len(q.Term.SuffixList) == 0 {
		if q.Term.Type == gojq.TermTypeQuery {
			q = q.Term.Query
			continue
		}
		return q.Term
	}
	return nil
}
