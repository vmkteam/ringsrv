package files

import (
	"context"
	"fmt"
	"image/color"
	"net/http"
	"testing"
	"time"

	"github.com/vmkteam/ringsrv/pkg/client/upstream"
	"github.com/vmkteam/ringsrv/pkg/ring/files/filestest"
	"github.com/vmkteam/ringsrv/pkg/ring/public"
	"github.com/vmkteam/ringsrv/pkg/ring/public/publictest"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	mmOpen    = publictest.ID("open")
	mmPrivate = publictest.ID("priv")
)

func mmChat(t *testing.T) *publictest.Mattermost {
	t.Helper()
	m := publictest.New(t)
	m.Channel(mmOpen, "O")
	m.Channel(mmPrivate, "P")
	return m
}

func mmProfile(t *testing.T, m *publictest.Mattermost, redactMode string) *target.Profile {
	t.Helper()
	extra := ""
	if m.Prefix != "" {
		extra = fmt.Sprintf("PathPrefix       = %q", m.Prefix)
	}
	toml := fmt.Sprintf(`
Env = "prod"

[Profiles.mattermost]
Description      = "prod · Mattermost"
BaseURL          = %q
Headers          = ["Authorization: Bearer mm-token"]
AllowMethods     = ["GET"]
AllowPaths       = ["^/api/v4/posts/[a-z0-9]{26}/thread$", "^/files/download$"]
AllowQueryParams = ["file", "offset", "limit<=1000", "grep", "sheet", "entry"]
Files            = "mattermost"
PublicOnly       = "mattermost"
Redact           = %q
%s

[Roles.viewer]
Description = "read"
Groups      = ["ringsrv-users"]
Tools       = ["api_call"]
Targets     = ["mattermost"]
`, m.URL(), redactMode, extra)
	c, err := target.Parse([]byte(toml))
	require.NoError(t, err)
	p, ok := c.Profile("mattermost")
	require.True(t, ok)
	return p
}

func mmFetch(t *testing.T, m *publictest.Mattermost, path string) (*Result, error) {
	t.Helper()
	client := upstream.New(upstream.Options{Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20})
	return NewFetcher(client, public.NewGuard(client)).Fetch(context.Background(), mmProfile(t, m, "warn"), path, 32<<10)
}

func TestMattermost_Text(t *testing.T) {
	t.Parallel()
	m := mmChat(t)
	id := publictest.ID("report")
	m.AddFile(id, mmOpen, "report.csv", "text/csv", []byte(report), nil)

	res, err := mmFetch(t, m, "/files/download?file="+id+"&grep=809000002")
	require.NoError(t, err)
	assert.Equal(t, Meta{ID: id, Name: "report.csv", FileType: "csv", Size: int64(len(report)), Channels: []string{mmOpen}}, res.Meta)
	assert.Equal(t, []string{"3: 809000002;Бинт;2026-12-01"}, res.Text.Lines)
	assert.Equal(t, "code;name;expires", res.Text.Header)
}

func TestMattermost_Workbook(t *testing.T) {
	t.Parallel()
	m := mmChat(t)
	id := publictest.ID("prices")
	m.AddFile(id, mmOpen, "prices.xlsx", mimeXLSX, prices(t), nil)
	pic := publictest.ID("pic")
	m.AddFile(pic, mmOpen, "shot.png", "image/png", filestest.PNG(t, 3, 3, color.White), nil)

	res, err := mmFetch(t, m, "/files/download?file="+id+"&sheet=2&limit=1")
	require.NoError(t, err)
	assert.Equal(t, []string{"1: code\tname\tprice\tuntil"}, res.Text.Lines)

	res, err = mmFetch(t, m, "/files/download?file="+pic)
	require.NoError(t, err)
	require.NotNil(t, res.Image)
}

// Whether a file is public comes before anything about it: a refused file
// is named by its id, and nothing of it is downloaded.
func TestMattermost_Refused(t *testing.T) {
	t.Parallel()
	m := mmChat(t)
	secret, deleted, huge, pdf := publictest.ID("secret"), publictest.ID("deleted"), publictest.ID("huge"), publictest.ID("pdf")
	m.AddFile(secret, mmPrivate, "salaries.csv", "text/csv", []byte("a\n"), nil)
	m.AddFile(deleted, mmOpen, "old.csv", "text/csv", []byte("a\n"), func(i map[string]any) { i["delete_at"] = 1 })
	m.AddFile(huge, mmOpen, "big.csv", "text/csv", []byte("a\n"), func(i map[string]any) { i["size"] = maxFileBytes + 1 })
	m.AddFile(pdf, mmOpen, "doc.pdf", "application/pdf", []byte("%PDF"), nil)

	for id, code := range map[string]string{secret: CodeNotPublic, deleted: CodeUnsupported, huge: CodeTooLarge, pdf: CodeUnsupported} {
		_, err := mmFetch(t, m, "/files/download?file="+id)
		require.Error(t, err, id)
		assert.Equal(t, code, refusalCode(err), err.Error())
		if code == CodeNotPublic {
			assert.NotContains(t, err.Error(), "salaries")
		}
	}
	assert.Zero(t, m.Downloads())

	_, err := mmFetch(t, m, "/files/download?file="+publictest.ID("nothing"))
	var up *UpstreamError
	require.ErrorAs(t, err, &up, "a file that is not there is the upstream's answer")
	assert.Equal(t, http.StatusNotFound, up.Status)

	_, err = mmFetch(t, m, "/files/download?file=F0123456789")
	require.ErrorIs(t, err, ErrBadParams, "a Slack id is not a Mattermost one")
}

func TestMattermost_SizeMismatch(t *testing.T) {
	t.Parallel()
	m := mmChat(t)
	id := publictest.ID("short")
	m.AddFile(id, mmOpen, "a.csv", "text/csv", []byte("a;b\n"), func(i map[string]any) { i["size"] = 99 })
	_, err := mmFetch(t, m, "/files/download?file="+id)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "download_mismatch")
}

// Behind a mount path the info, the channel and the bytes all come through it.
func TestMattermost_PathPrefix(t *testing.T) {
	t.Parallel()
	m := mmChat(t)
	m.Prefix = "/plugins/com.example.facade"
	id := publictest.ID("report")
	m.AddFile(id, mmOpen, "report.csv", "text/csv", []byte(report), nil)

	res, err := mmFetch(t, m, "/files/download?file="+id+"&limit=1")
	require.NoError(t, err)
	assert.Equal(t, []string{"1: code;name;expires"}, res.Text.Lines)
	assert.Equal(t, []string{"GET /api/v4/files/" + id + "/info", "GET /api/v4/channels/" + mmOpen, "GET /api/v4/files/" + id}, m.Requests())
}
