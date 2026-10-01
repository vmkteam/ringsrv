package files

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"

	"github.com/vmkteam/ringsrv/pkg/client/upstream"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/vmkteam/mcpkit/mcp"
	"github.com/vmkteam/mcpkit/redact"
)

// slackFileHost is where Slack serves file bytes. Fixed here: the download URL
// comes out of an answer, and an answer does not get to choose a host.
const slackFileHost = "files.slack.com"

// slackInfoMaxBytes caps the files.info answer, read whole. It is metadata, a
// few kilobytes: the cap only bounds an answer that is broken.
const slackInfoMaxBytes = 4 << 20

// slackInfo is the part of files.info this route reads. Nothing else of the
// answer travels on.
type slackInfo struct {
	OK    bool      `json:"ok"`
	Error string    `json:"error"`
	File  slackFile `json:"file"`
}

type slackFile struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Mimetype    string   `json:"mimetype"`
	Filetype    string   `json:"filetype"`
	Mode        string   `json:"mode"`
	Size        int64    `json:"size"`
	IsPublic    bool     `json:"is_public"`
	IsExternal  bool     `json:"is_external"`
	DownloadURL string   `json:"url_private_download"`
	Channels    []string `json:"channels"`
	Shares      struct {
		Public map[string]json.RawMessage `json:"public"`
	} `json:"shares"`
}

// slack serves a Slack file: files.info, the checks, the download.
func (f *Fetcher) slack(ctx context.Context, prof *target.Profile, p params, budget int) (*Result, error) {
	t := prof.Upstream()
	file, err := f.slackInfo(ctx, t, p.File)
	if err != nil {
		return nil, err
	}
	meta, kind, err := checkSlackFile(file, p.File, prof.Redact)
	if err != nil {
		return nil, err
	}
	data, err := f.slackDownload(ctx, t, file)
	if err != nil {
		return nil, err
	}
	if kind == kindImage {
		img, imgErr := f.picture(ctx, data)
		if imgErr != nil {
			return nil, imgErr
		}
		return &Result{Meta: meta, Image: img}, nil
	}
	text, err := textOf(meta, data, p, budget)
	if err != nil {
		return nil, err
	}
	return &Result{Meta: meta, Text: text}, nil
}

func (f *Fetcher) slackInfo(ctx context.Context, t upstream.Target, id string) (slackFile, error) {
	resp, err := f.client.Do(ctx, t, upstream.Request{
		Method: http.MethodGet, Path: "/api/files.info?file=" + url.QueryEscape(id), MaxBytes: slackInfoMaxBytes,
	})
	switch {
	case err != nil:
		return slackFile{}, err
	case resp.Status != http.StatusOK:
		return slackFile{}, answered(resp.Status, "slack files.info answered %d", resp.Status)
	case resp.Truncated:
		return slackFile{}, answered(resp.Status, "slack files.info answer is over %d bytes", slackInfoMaxBytes)
	}
	var info slackInfo
	if err := json.Unmarshal(resp.Body, &info); err != nil {
		return slackFile{}, answered(resp.Status, "slack files.info: %s", err)
	}
	if !info.OK {
		// file_not_found, missing_scope and the like say what is wrong as they are.
		return slackFile{}, answered(resp.Status, "slack files.info: %s", info.Error)
	}
	return info.File, nil
}

// File kinds the route can show.
const (
	kindText  = "text"
	kindImage = "image"
)

// checkSlackFile decides whether the file may be served and how, before a byte
// of it is downloaded. The order matters: a file that is not public is refused
// before anything about it — name, size, type — can show up in a message.
func checkSlackFile(file slackFile, id, redactMode string) (Meta, string, error) {
	if file.ID != id {
		return Meta{}, "", answered(http.StatusOK, "slack files.info answered for %q, not %q", file.ID, id)
	}
	public := publicChannels(file)
	if !file.IsPublic || len(public) == 0 {
		return Meta{}, "", refused(CodeNotPublic,
			"file %s is not shared in a public channel; only files of public channels are served", id)
	}
	meta := Meta{ID: file.ID, Name: file.Name, FileType: file.Filetype, Size: file.Size, Channels: public}
	// What the file is comes before how big it is: an archive is refused as
	// one, not as too large, which would read as "a smaller one would do".
	kind := classify(file.Name, file.Mimetype, file.Mode)
	switch {
	case file.IsExternal, file.Mode != "hosted" && file.Mode != "snippet", file.DownloadURL == "":
		return meta, "", refused(CodeUnsupported,
			"file %s (%s) has no bytes this server can download: it is %s", id, file.Filetype, describeMode(file))
	case kind == "":
		return meta, "", refused(CodeUnsupported,
			"file %s is %s (%s): only text and %s images are served", id, file.Filetype, file.Mimetype, imageKinds)
	case kind == kindImage && redactMode == redact.ModeOn:
		// A picture cannot be masked, and this target promises masking.
		return meta, "", refused(CodeUnsupported,
			"file %s is an image, and this target masks personal data, which a picture does not allow", id)
	case file.Size > maxFileBytes:
		return meta, "", refused(CodeTooLarge,
			"file %s is %d bytes, over the %d this server downloads", id, file.Size, maxFileBytes)
	}
	return meta, kind, nil
}

// publicChannels are the public channels a file is shared in, by both of the
// fields Slack reports them in. Private channels and direct messages are never
// evidence of anything here.
func publicChannels(file slackFile) []string {
	set := make(map[string]bool, len(file.Channels)+len(file.Shares.Public))
	for _, c := range file.Channels {
		set[c] = true
	}
	for c := range file.Shares.Public {
		set[c] = true
	}
	delete(set, "")
	return slices.Sorted(maps.Keys(set))
}

func describeMode(file slackFile) string {
	switch {
	case file.IsExternal:
		return "a link to an external file"
	case file.DownloadURL == "" && file.Mode != "":
		return "a " + file.Mode + " without a download link"
	default:
		return "a " + file.Mode
	}
}

// classify picks the kind of a file from what Slack says it is: a snippet is
// always text, whatever its type. A type Slack left empty is nothing: the
// shared check reads an empty type as text, which is for a source that has no
// type to give.
//
// A file Slack did not recognise — an HTML report comes as "binary",
// application/octet-stream — is text when its name says so. Whatever the name,
// a NUL near the start still refuses it once the bytes are in.
func classify(name, mimetype, mode string) string {
	mt := mediaType(mimetype)
	format, isImage := strings.CutPrefix(mt, "image/")
	switch {
	case mode == "snippet", mt == "application/x-yaml", mt != "" && mcp.IsTextualMIME(mt):
		return kindText
	case isImage && imageFormats[format]:
		return kindImage
	case (mt == "" || mt == "application/octet-stream") && textExtensions[strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))]:
		return kindText
	}
	return ""
}

// textExtensions are the names of text files whose type Slack does not know.
var textExtensions = map[string]bool{
	"csv": true, "tsv": true, "txt": true, "log": true, "json": true, "yaml": true, "yml": true,
	"md": true, "html": true, "htm": true, "xml": true, "sql": true,
}

// mediaType is a Content-Type without its parameters, lower case.
func mediaType(contentType string) string {
	mt, _, _ := strings.Cut(strings.ToLower(contentType), ";")
	return strings.TrimSpace(mt)
}

// slackDownload fetches the file and checks that what came is the file: the
// size files.info announced, and not a page. Without the token the file host
// answers with a redirect to the sign-in page, which is never followed.
func (f *Fetcher) slackDownload(ctx context.Context, t upstream.Target, file slackFile) ([]byte, error) {
	resp, err := f.client.Download(ctx, t, file.DownloadURL, upstream.DownloadOptions{
		Hosts: []string{slackFileHost}, MaxBytes: file.Size + 1,
	})
	switch {
	case err != nil:
		return nil, err
	case resp.Status >= 300 && resp.Status < 400:
		return nil, answered(resp.Status, "slack download of %s answered %d with a redirect — the token was not accepted for the file", file.ID, resp.Status)
	case resp.Status != http.StatusOK:
		return nil, answered(resp.Status, "slack download of %s answered %d", file.ID, resp.Status)
	case resp.Truncated, int64(len(resp.Body)) != file.Size:
		return nil, answered(resp.Status, "download_mismatch: %s came as %d bytes, files.info says %d", file.ID, len(resp.Body), file.Size)
	case isHTML(resp.ContentType) && file.Filetype != "html":
		return nil, answered(resp.Status, "download_mismatch: %s came as an HTML page, not as a %s file", file.ID, file.Filetype)
	}
	return resp.Body, nil
}

func isHTML(contentType string) bool { return mediaType(contentType) == "text/html" }
