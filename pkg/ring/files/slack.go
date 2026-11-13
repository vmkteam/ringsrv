package files

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"slices"

	"github.com/vmkteam/ringsrv/pkg/client/upstream"
	"github.com/vmkteam/ringsrv/pkg/ring/target"
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

// slack finds a Slack file: files.info, the checks, the download.
func (f *Fetcher) slack(ctx context.Context, prof *target.Profile, p params) (source, error) {
	t := prof.Upstream()
	file, err := f.slackInfo(ctx, t, p.File)
	if err != nil {
		return source{}, err
	}
	meta, kind, err := checkSlackFile(file, p.File, prof.Redact)
	if err != nil {
		return source{}, err
	}
	data, err := f.slackDownload(ctx, t, file)
	if err != nil {
		return source{}, err
	}
	return source{meta: meta, kind: kind, data: data}, nil
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
	if file.IsExternal || file.Mode != "hosted" && file.Mode != "snippet" || file.DownloadURL == "" {
		return meta, "", refused(CodeUnsupported,
			"file %s (%s) has no bytes this server can download: it is %s", id, file.Filetype, describeMode(file))
	}
	kind := slackKind(file)
	if err := admit(meta, kind, file.Mimetype, redactMode); err != nil {
		return meta, "", err
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

// slackKind is the kind of a Slack file: a snippet is always text, whatever
// its type. A type Slack left empty is nothing — the shared classify would read
// it by the name, and Slack names the type of every file it holds. A file
// Slack did not recognise — an HTML report comes as "binary",
// application/octet-stream — goes by its name.
func slackKind(file slackFile) string {
	switch {
	case file.Mode == "snippet":
		return kindText
	case file.Mimetype == "":
		return ""
	}
	return classify(file.Name, file.Mimetype)
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
