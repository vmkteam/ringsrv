// Package files serves the file route of a catalogue profile (target.FilesPath).
// The model names a file by its id; the file is looked up through the
// upstream's own API, refused unless it is public and of a kind this server can
// show, downloaded from the host the provider pins, and handed back as
// numbered lines or as an image. A spreadsheet comes as the lines of one sheet,
// an archive as its table of contents or as one of its files, shown the same
// way.
//
// A provider finds and checks the file and hands over its bytes (source); how
// the bytes are shown does not depend on where they came from (render).
//
// The lookup's answer never reaches the model as it came: with the scope the
// lookup needs, the upstream describes files from private channels and direct
// messages too, so only what passed the checks is put into the answer.
package files

import (
	"context"
	"errors"
	"fmt"

	"github.com/vmkteam/ringsrv/pkg/client/upstream"
	"github.com/vmkteam/ringsrv/pkg/ring/target"
)

// Refusal codes, the ones the model reads in the error.
const (
	CodeNotPublic   = "FileNotPublic"
	CodeTooLarge    = "FileTooLarge"
	CodeUnsupported = "UnsupportedFileType"
)

// maxFileBytes is the largest file the route downloads.
const maxFileBytes = 10 << 20

// RefusedError is a file this route will not serve. The message says why and
// what to do instead; for a file that is not public it names nothing but the id.
type RefusedError struct {
	Code    string
	Message string
}

func (e *RefusedError) Error() string { return e.Message }

func refused(code, format string, args ...any) error {
	return &RefusedError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// ErrBadParams marks a call whose parameters the route cannot use.
var ErrBadParams = errors.New("bad parameters")

// UpstreamError is a failure the upstream reported: it answered, with Status,
// and said no or served something other than the file. Not a refusal of the
// route, and not this server's failure either.
type UpstreamError struct {
	Status int
	Err    error
}

func (e *UpstreamError) Error() string { return e.Err.Error() }
func (e *UpstreamError) Unwrap() error { return e.Err }

func answered(status int, format string, args ...any) error {
	return &UpstreamError{Status: status, Err: fmt.Errorf(format, args...)}
}

// Meta is what the file is, for the answer and for the audit. Channels are the
// public ones only.
type Meta struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	FileType string   `json:"filetype"`
	Size     int64    `json:"size"`
	Channels []string `json:"channels"`
	// Entry is the file inside an archive that was shown, when the file is
	// one and a file of it was asked for.
	Entry string `json:"entry,omitempty"`
}

// Text is a text file as numbered lines, the way code_read numbers them.
type Text struct {
	Meta
	// Encoding is set when the file was not UTF-8 and was decoded from it.
	Encoding string `json:"encoding,omitempty"`
	// Sheet is the sheet of a workbook the lines are from; Sheets names all
	// of its sheets, in their order.
	Sheet  string   `json:"sheet,omitempty"`
	Sheets []string `json:"sheets,omitempty"`
	// Header is the column names of a table when the lines below do not
	// include them: line 1 of a table read from the middle or through grep,
	// and always for an archive's table of contents.
	Header     string `json:"header,omitempty"`
	TotalLines int    `json:"total_lines"`
	LineFrom   int    `json:"line_from,omitempty"`
	LineTo     int    `json:"line_to,omitempty"`
	Grep       string `json:"grep,omitempty"`
	// Matched counts every matching line from offset on, returned or not.
	Matched   *int `json:"matched,omitempty"`
	Truncated bool `json:"truncated"`
	// NextOffset is the offset that continues where this answer stopped.
	NextOffset int      `json:"next_offset,omitempty"`
	Lines      []string `json:"lines"`
}

// Image is a picture ready for an image content block: encoded bytes, not
// base64.
type Image struct {
	MimeType string
	Data     []byte
	Width    int
	Height   int
}

// Result is one served file: Text or Image is set.
type Result struct {
	Meta  Meta
	Text  *Text
	Image *Image
}

// Fetcher serves the route for every Files profile of an instance.
type Fetcher struct {
	client *upstream.Client
	// decodes bounds how many images are decoded at once: a batch runs four
	// calls together, and one decode may take maxDecodeBytes.
	decodes chan struct{}
}

// maxDecodes is how many images one instance decodes at a time. One: the
// smallest contour has 256 MB, and two pictures at the decode limit would be
// half of it.
const maxDecodes = 1

// NewFetcher builds the route over the instance's upstream client: the same
// concurrency limit and transport as every other call.
func NewFetcher(client *upstream.Client) *Fetcher {
	return &Fetcher{client: client, decodes: make(chan struct{}, maxDecodes)}
}

// Fetch serves one call of the route. budget is the size the text answer has
// to fit in, envelope included.
func (f *Fetcher) Fetch(ctx context.Context, prof *target.Profile, rawPath string, budget int) (*Result, error) {
	var load func(context.Context, *target.Profile, params) (source, error)
	switch prof.Files {
	case target.FilesSlack:
		load = f.slack
	default:
		// The catalogue refuses an unknown provider at load time.
		return nil, fmt.Errorf("files: profile %q names no provider this server knows", prof.Name())
	}
	p, err := parseParams(rawPath, fileIDs[prof.Files])
	if err != nil {
		return nil, err
	}
	src, err := load(ctx, prof, p)
	if err != nil {
		return nil, err
	}
	return f.render(ctx, src, p, budget, prof.Redact)
}
