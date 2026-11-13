package files

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/vmkteam/ringsrv/pkg/client/upstream"
	"github.com/vmkteam/ringsrv/pkg/ring/target"
)

// mmInfoMaxBytes caps the file info answer: metadata, a few kilobytes.
const mmInfoMaxBytes = 1 << 20

// mmInfo is the part of a Mattermost FileInfo this route reads. Nothing else
// of the answer travels on.
type mmInfo struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	Name      string `json:"name"`
	Extension string `json:"extension"`
	Size      int64  `json:"size"`
	MimeType  string `json:"mime_type"`
	DeleteAt  int64  `json:"delete_at"`
	Archived  bool   `json:"archived"`
}

// mattermost finds a Mattermost file: its info, the type of the channel it
// was posted in, the download — all through the profile's own API, prefix
// included, so the bytes come from the host the info came from.
func (f *Fetcher) mattermost(ctx context.Context, prof *target.Profile, p params) (source, error) {
	if f.channels == nil {
		return source{}, errors.New("files: no channel check for a mattermost profile")
	}
	t := prof.Upstream()
	info, err := f.mmInfo(ctx, t, p.File)
	if err != nil {
		return source{}, err
	}
	// Whether it is public comes before anything about it — name, size,
	// type — can show up in a message.
	public, err := f.channels.ChannelPublic(ctx, prof, info.ChannelID)
	if err != nil {
		return source{}, err
	}
	if !public {
		return source{}, notPublic(p.File)
	}
	meta := Meta{ID: info.ID, Name: info.Name, FileType: strings.ToLower(info.Extension), Size: info.Size, Channels: []string{info.ChannelID}}
	if info.DeleteAt > 0 || info.Archived {
		return source{}, refused(CodeUnsupported, "file %s is deleted or its content is no longer kept", p.File)
	}
	kind := classify(info.Name, info.MimeType)
	if err = admit(meta, kind, info.MimeType, prof.Redact); err != nil {
		return source{}, err
	}
	data, err := f.mmDownload(ctx, t, info)
	if err != nil {
		return source{}, err
	}
	return source{meta: meta, kind: kind, data: data}, nil
}

func notPublic(id string) error {
	return refused(CodeNotPublic, "file %s is not in a public channel; only files of public channels are served", id)
}

// mmInfo reads the file's info. A file the credential may not see is a file
// that is not public: a public channel is readable by every member of its
// team.
func (f *Fetcher) mmInfo(ctx context.Context, t upstream.Target, id string) (mmInfo, error) {
	resp, err := f.client.Do(ctx, t, upstream.Request{
		Method: http.MethodGet, Path: "/api/v4/files/" + id + "/info", MaxBytes: mmInfoMaxBytes,
	})
	switch {
	case err != nil:
		return mmInfo{}, err
	case resp.Status == http.StatusForbidden:
		return mmInfo{}, notPublic(id)
	case resp.Status != http.StatusOK:
		return mmInfo{}, answered(resp.Status, "mattermost file info of %s answered %d", id, resp.Status)
	case resp.Truncated:
		return mmInfo{}, answered(resp.Status, "mattermost file info of %s is over %d bytes", id, mmInfoMaxBytes)
	}
	var info mmInfo
	if err := json.Unmarshal(resp.Body, &info); err != nil {
		return mmInfo{}, answered(resp.Status, "mattermost file info of %s: %s", id, err)
	}
	if info.ID != id {
		return mmInfo{}, answered(resp.Status, "mattermost file info answered for %q, not %q", info.ID, id)
	}
	return info, nil
}

// mmDownload fetches the file and checks that what came is the file: the size
// the info announced, and not a page.
func (f *Fetcher) mmDownload(ctx context.Context, t upstream.Target, info mmInfo) ([]byte, error) {
	resp, err := f.client.Do(ctx, t, upstream.Request{
		Method: http.MethodGet, Path: "/api/v4/files/" + info.ID, MaxBytes: info.Size + 1,
	})
	if err != nil {
		return nil, err
	}
	if err := checkDownload(resp, "mattermost", info.ID, info.Size, mediaType(info.MimeType) == "text/html"); err != nil {
		return nil, err
	}
	return resp.Body, nil
}
