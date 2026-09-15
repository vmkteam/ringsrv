// Package codegraph talks to code-graph-mcp, the AST engine: it stores symbol
// boundaries and bodies and answers from a real call graph rather than from a
// grep over names.
//
// It is spoken to over MCP stdio rather than its CLI, which prints for humans. A
// whole call — spawn, initialize, one tool, exit — costs 0.04s measured, so
// there is no process pool: one process per question, like the git client.
package codegraph

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Errors the domain distinguishes.
var (
	ErrNotFound  = errors.New("codegraph: not found")
	ErrNotIndex  = errors.New("codegraph: repository is not indexed")
	ErrBadSymbol = errors.New("codegraph: invalid symbol")
	// ErrIndexing is the engine catching up in the background. It is not a
	// failure and never reaches the caller: call() waits it out.
	ErrIndexing = errors.New("codegraph: index is still being built")
)

// AmbiguousError is the engine refusing to guess between two symbols of the same
// name. An error rather than an empty answer on purpose: the engine reports it
// inside a successful response, where decoding it naively yields "nobody calls
// this".
type AmbiguousError struct {
	Symbol     string
	Candidates []Node
}

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("codegraph: %q is ambiguous, pass path to choose: %s", e.Symbol, strings.Join(e.Paths(), ", "))
}

// Paths names the candidates as path:line. The tool error carries the same
// list, so it is built once here rather than formatted again at the call site.
func (e *AmbiguousError) Paths() []string {
	res := make([]string, 0, len(e.Candidates))
	for _, c := range e.Candidates {
		res = append(res, fmt.Sprintf("%s:%d", c.Path, c.StartLine))
	}
	return res
}

// IndexDir is where the engine keeps the index — inside the tree it indexed, so
// removing a worktree removes its index with it, which is what the LRU wants.
const IndexDir = ".code-graph"

const defaultBin = "code-graph-mcp"

// symbolRe is what may be asked about. Not a shell-injection guard — the name
// travels as JSON — but it keeps megabyte-long junk out of an index query.
var symbolRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]{0,200}$`)

// Options configure the client.
type Options struct {
	// Bin is the engine binary; empty means "code-graph-mcp" from PATH.
	Bin     string
	Timeout time.Duration
}

// Client runs the engine. Safe for concurrent use.
type Client struct {
	opts Options
	indexState
}

func New(opts Options) *Client {
	if opts.Bin == "" {
		opts.Bin = defaultBin
	}
	if opts.Timeout <= 0 {
		opts.Timeout = time.Minute
	}
	return &Client{opts: opts}
}

// Version reports the engine version and doubles as the boot check: a missing
// binary is a startup error rather than a puzzle on the first question.
func (c *Client) Version(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, c.opts.Bin, "--version").CombinedOutput() //nolint:gosec // fixed argv
	if err != nil {
		return "", fmt.Errorf("codegraph: %s --version: %w: %s", c.opts.Bin, err, bytes.TrimSpace(out))
	}
	return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0]), nil
}

// Node is one symbol. Boundaries are the reason this engine was chosen: with
// start and end a stack frame maps onto a symbol exactly, instead of by the
// "last symbol above the line" approximation.
type Node struct {
	Name      string `json:"name"`
	Qualified string `json:"qualified_name"`
	// Receiver is the type a method belongs to. The engine does not report
	// it; GoFileSymbols does.
	Receiver  string `json:"receiver,omitempty"`
	Type      string `json:"type"`
	Path      string `json:"file_path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Signature string `json:"signature"`
	Body      string `json:"code_content"`
}

// Reference is one site that calls, imports or implements a symbol.
type Reference struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Path      string `json:"file_path"`
	StartLine int    `json:"start_line"`
	Relation  string `json:"relation"`
	// Confidence is the engine's own word for how sure it is: "inferred" means
	// the edge was resolved by name, which in Go can confuse two methods of
	// different types. It travels to the answer rather than being smoothed over.
	Confidence string `json:"confidence"`
}

// call runs one MCP tool in dir and decodes the JSON the tool returned.
func (c *Client) call(ctx context.Context, dir, tool string, args map[string]any, dst any) error {
	if !c.Indexed(dir) {
		return fmt.Errorf("%w: %s", ErrNotIndex, dir)
	}

	ctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()

	req, err := requests(tool, args)
	if err != nil {
		return err
	}

	// The engine catches up on its index in the background and refuses queries
	// while it does. That is its business, not the caller's: a model told to
	// retry asks a different tool instead, and the answer turns into a guess.
	err = c.exec(ctx, dir, tool, req, dst)
	if !errors.Is(err, ErrIndexing) {
		return err
	}

	// Waiting does not help: every attempt starts a fresh process and the engine
	// restarts the same unfinished pass. Finishing it once, as its own command,
	// does.
	if ferr := c.Finalize(ctx, dir); ferr != nil {
		return fmt.Errorf("%w: %w", ErrIndexing, ferr)
	}
	return c.exec(ctx, dir, tool, req, dst)
}

func (c *Client) exec(ctx context.Context, dir, tool string, req []byte, dst any) error {
	cmd := exec.CommandContext(ctx, c.opts.Bin, "serve") //nolint:gosec // fixed argv; arguments travel as JSON on stdin
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(req)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("codegraph: %s: %w: %s", tool, err, trimErr(stderr.String()))
	}
	return decodeToolResult(stdout.Bytes(), tool, dst)
}

// callID is the id of the tools/call request; initialize takes 1.
const callID = 3

func requests(tool string, args map[string]any) ([]byte, error) {
	if args == nil {
		args = map[string]any{}
	}
	call, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": callID, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	if err != nil {
		return nil, fmt.Errorf("codegraph: marshal call: %w", err)
	}

	var b bytes.Buffer
	b.WriteString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"ringsrv","version":"1"}}}` + "\n")
	b.WriteString(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")
	b.Write(call)
	b.WriteByte('\n')
	return b.Bytes(), nil
}

// decodeToolResult digs the tool's payload out of the MCP envelope: the answer
// is JSON inside content[0].text, which is where MCP puts everything.
func decodeToolResult(out []byte, tool string, dst any) error {
	type envelope struct {
		ID     int `json:"id"`
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}

	for line := range bytes.SplitSeq(out, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var env envelope
		if err := json.Unmarshal(line, &env); err != nil || env.ID != callID {
			continue
		}
		switch {
		case env.Error != nil:
			return fmt.Errorf("codegraph: %s: %s", tool, env.Error.Message)
		case len(env.Result.Content) == 0:
			return fmt.Errorf("codegraph: %s: empty answer", tool)
		case env.Result.IsError:
			return toolError(tool, env.Result.Content[0].Text)
		}
		payload := []byte(env.Result.Content[0].Text)
		if err := payloadError(payload); err != nil {
			return err
		}
		if err := json.Unmarshal(payload, dst); err != nil {
			return fmt.Errorf("codegraph: %s: decode: %w", tool, err)
		}
		return nil
	}
	return fmt.Errorf("codegraph: %s: no answer", tool)
}

// toolError maps the engine's own refusals onto ours: "not found" is an answer,
// "the engine broke" is an incident, and the two must stay apart.
func toolError(tool, text string) error {
	if isIndexing(text) {
		return ErrIndexing
	}
	if strings.Contains(strings.ToLower(text), "not found") || strings.Contains(text, "No symbol") {
		return fmt.Errorf("%w: %s", ErrNotFound, strings.TrimSpace(text))
	}
	return fmt.Errorf("codegraph: %s: %s", tool, strings.TrimSpace(text))
}

// payloadError catches the refusals the engine reports inside a successful
// answer: an ambiguous name comes back as {"error": …, "suggestions": […]} with
// no isError flag, which decodes as an empty list of references.
func payloadError(payload []byte) error {
	var refusal struct {
		Error       string `json:"error"`
		Symbol      string `json:"symbol"`
		Suggestions []Node `json:"suggestions"`
	}
	// A payload that is not this shape is a normal answer, not a refusal, so a
	// decode failure here means "nothing to report" rather than an error.
	if err := json.Unmarshal(payload, &refusal); err != nil {
		return nil //nolint:nilerr // the shape did not match: not a refusal
	}
	if refusal.Error == "" {
		return nil
	}
	if isIndexing(refusal.Error) {
		return ErrIndexing
	}
	if len(refusal.Suggestions) > 0 {
		return &AmbiguousError{Symbol: refusal.Symbol, Candidates: refusal.Suggestions}
	}
	if strings.Contains(strings.ToLower(refusal.Error), "not found") {
		return fmt.Errorf("%w: %s", ErrNotFound, refusal.Error)
	}
	return fmt.Errorf("codegraph: %s", refusal.Error)
}

// isIndexing recognises the engine's own wording for "come back later".
func isIndexing(text string) bool {
	return strings.Contains(strings.ToLower(text), "indexing in progress")
}

// trimErr caps a subprocess's stderr for a log line: enough to see the message,
// not enough to be a data channel.
func trimErr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > errBytes {
		return s[:errBytes] + "…"
	}
	return s
}

// errBytes is how much of a failing subprocess's output travels into a log.
const errBytes = 300

// ValidateSymbol reports whether a name may be asked about.
func ValidateSymbol(name string) error {
	if !symbolRe.MatchString(name) {
		return fmt.Errorf("%w: %q", ErrBadSymbol, name)
	}
	return nil
}

func indexPath(dir string) string { return filepath.Join(dir, IndexDir, "index.db") }
