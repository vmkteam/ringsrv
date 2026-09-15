package ring

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/vmkteam/mcpkit/mcp"
	"github.com/vmkteam/mcpkit/redact"
)

// MaxAnswerBytes is the most a call may ask for in max_bytes. The target and
// the instance defaults are the operator's numbers and are not capped here;
// the argument is the model's, and without a ceiling it lifted the answer to
// whatever the client read from the upstream.
const MaxAnswerBytes = 256 << 10

// PickLimit picks the answer size cap: the call wins over the target, the
// target over the instance default, and the call never wins past
// MaxAnswerBytes.
func PickLimit(call, target, instance int) int {
	switch {
	case call > MaxAnswerBytes:
		return MaxAnswerBytes
	case call > 0:
		return call
	case target > 0:
		return target
	default:
		return instance
	}
}

// Shaped is a JSON answer after the filter, the redaction and the size
// limit. Either Data or Text carries the body: Text is what an oversized
// answer travels as, because cutting JSON stops it from being JSON and a
// parse error is not something the model can act on.
type Shaped struct {
	Data       any
	Text       string
	AsText     bool
	Truncated  bool
	BytesTotal int
	Redacted   redact.Result
}

// ShapeJSON runs the filter, the redaction and the size limit over a JSON body,
// in that order: jq first, because it makes the body smaller; redaction over
// what is left; the cut last, over the encoded text. It is the one pipeline an
// HTTP answer and a query result share. A jq failure comes back as the ApplyJQ
// error, and the caller adds the keys the next attempt needs.
func ShapeJSON(ctx context.Context, body []byte, expr string, jqTimeout time.Duration, mode string, rules []string, limit int) (Shaped, error) {
	filtered, err := ApplyJQ(ctx, expr, body, jqTimeout)
	if err != nil {
		return Shaped{}, err
	}
	filtered, red := redact.Value(filtered, redact.Options{Mode: mode, Rules: rules})

	encoded, err := json.Marshal(filtered)
	if err != nil {
		return Shaped{}, fmt.Errorf("encode filtered body: %w", err)
	}
	if limit > 0 && len(encoded) > limit {
		text, _, total := mcp.Truncate(string(encoded), limit)
		return Shaped{Text: text, AsText: true, Truncated: true, BytesTotal: total, Redacted: red}, nil
	}
	return Shaped{Data: filtered, BytesTotal: len(encoded), Redacted: red}, nil
}
