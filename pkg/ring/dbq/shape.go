package dbq

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/vmkteam/ringsrv/pkg/ring"
)

// body is the object the answer is shaped from: the
// column names once, then rows as arrays in that order (D3).
type body struct {
	Columns      []jsonColumn `json:"columns"`
	Rows         [][]any      `json:"rows"`
	Truncated    bool         `json:"truncated"`
	RowsReturned int          `json:"rows_returned"`
	ElapsedMS    int64        `json:"elapsed_ms"`
}

type jsonColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// shape encodes the result and runs it through the pipeline an HTTP answer
// goes through (ring.ShapeJSON): jq, the target's redaction, the size cap.
func (m *Manager) shape(ctx context.Context, e *entry, req Request, res *Result, truncated bool, elapsed time.Duration) (*Answer, *Error) {
	b := body{
		Columns:      make([]jsonColumn, len(res.Columns)),
		Rows:         make([][]any, len(res.Rows)),
		Truncated:    truncated,
		RowsReturned: len(res.Rows),
		ElapsedMS:    elapsed.Milliseconds(),
	}
	for i, c := range res.Columns {
		b.Columns[i] = jsonColumn(c)
	}
	for i, row := range res.Rows {
		b.Rows[i] = make([]any, len(row))
		for j, v := range row {
			b.Rows[i][j] = Normalize(v)
		}
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return nil, &Error{Kind: FailUpstream, Err: fmt.Errorf("encode result: %w", err)}
	}

	limit := ring.PickLimit(req.MaxBytes, e.MaxBytes, m.opts.MaxBytes)
	shaped, err := ring.ShapeJSON(ctx, raw, req.JQ, m.opts.JQTimeout, e.Redact, e.RedactRules, limit)
	if err != nil {
		return nil, &Error{Kind: FailJQ, Err: err, Keys: ring.TopLevelKeys(raw)}
	}
	return &Answer{
		Data: shaped.Data, Text: shaped.Text, AsText: shaped.AsText,
		Truncated: truncated || shaped.Truncated, BytesTotal: shaped.BytesTotal, Redacted: shaped.Redacted,
		RowsReturned: len(res.Rows), Elapsed: elapsed,
	}, nil
}
