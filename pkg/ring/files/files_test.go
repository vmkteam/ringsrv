package files

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vmkteam/ringsrv/pkg/client/upstream"
	"github.com/vmkteam/ringsrv/pkg/ring/files/filestest"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func profileFor(t *testing.T, f *filestest.Slack, redactMode string) *target.Profile {
	t.Helper()
	toml := fmt.Sprintf(`
Env = "prod"

[Profiles.chat]
Description      = "prod · Chat"
BaseURL          = %q
Headers          = ["Authorization: Bearer chat-token"]
AllowMethods     = ["GET"]
AllowPaths       = ["^/files/download$"]
AllowQueryParams = ["file", "offset", "limit<=1000", "grep"]
Files            = "slack"
Redact           = %q

[Roles.viewer]
Description = "read"
Groups      = ["ringsrv-users"]
Tools       = ["api_call"]
Targets     = ["chat"]
`, f.URL(), redactMode)
	c, err := target.Parse([]byte(toml))
	require.NoError(t, err)
	p, ok := c.Profile("chat")
	require.True(t, ok)
	return p
}

func fetch(t *testing.T, f *filestest.Slack, path string, budget int) (*Result, error) {
	t.Helper()
	client := upstream.New(upstream.Options{Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20})
	return NewFetcher(client).Fetch(context.Background(), profileFor(t, f, "warn"), path, budget)
}

func refusalCode(err error) string {
	var ref *RefusedError
	if errors.As(err, &ref) {
		return ref.Code
	}
	return ""
}

const report = "code;name;expires\n809000001;Аспирин;2027-01-01\n809000002;Бинт;2026-12-01\n809000003;аспирин детский;2027-03-01\n"

func TestFetch_TextGrep(t *testing.T) {
	t.Parallel()
	f := filestest.NewSlack(t)
	f.Add("F0000000001", "report.csv", "csv", "text/csv", "snippet", []byte(report), nil)

	res, err := fetch(t, f, "/files/download?file=F0000000001&grep=%D0%90%D0%A1%D0%9F%D0%98%D0%A0%D0%98%D0%9D", 32<<10)
	require.NoError(t, err)
	require.NotNil(t, res.Text)
	txt := res.Text
	assert.Equal(t, []string{"2: 809000001;Аспирин;2027-01-01", "4: 809000003;аспирин детский;2027-03-01"}, txt.Lines,
		"grep ignores case, Cyrillic included, and keeps the line numbers")
	require.NotNil(t, txt.Matched)
	assert.Equal(t, 2, *txt.Matched)
	assert.Equal(t, "code;name;expires", txt.Header, "a table searched by grep keeps its column names")
	assert.Equal(t, 4, txt.TotalLines)
	assert.Equal(t, Meta{ID: "F0000000001", Name: "report.csv", FileType: "csv", Size: int64(len(report)), Channels: []string{"C0123456789"}}, res.Meta)
	assert.Equal(t, "Bearer chat-token", f.Auth(), "the download carries the profile's credential")
}

func TestFetch_TextWindow(t *testing.T) {
	t.Parallel()
	f := filestest.NewSlack(t)
	var b strings.Builder
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&b, "line %d\r\n", i)
	}
	f.Add("F0000000002", "app.log", "text", "text/plain", "hosted", []byte(b.String()), nil)

	res, err := fetch(t, f, "/files/download?file=F0000000002&offset=10&limit=5", 32<<10)
	require.NoError(t, err)
	txt := res.Text
	assert.Equal(t, []string{"11: line 11", "12: line 12", "13: line 13", "14: line 14", "15: line 15"}, txt.Lines, "\\r of \\r\\n is dropped")
	assert.Equal(t, 50, txt.TotalLines)
	assert.Equal(t, 11, txt.LineFrom)
	assert.Equal(t, 15, txt.LineTo)
	assert.Equal(t, 15, txt.NextOffset, "more lines follow the window")
	assert.False(t, txt.Truncated, "stopped by limit, not by size")
	assert.Empty(t, txt.Header, "not a table")

	res, err = fetch(t, f, "/files/download?file=F0000000002&offset=45", 32<<10)
	require.NoError(t, err)
	assert.Len(t, res.Text.Lines, 5)
	assert.Zero(t, res.Text.NextOffset, "the end of the file")
}

// The answer is cut line by line to the budget, with the offset that goes on.
func TestFetch_TextBudget(t *testing.T) {
	t.Parallel()
	f := filestest.NewSlack(t)
	body := strings.Repeat(strings.Repeat("x", 100)+"\n", 200)
	f.Add("F0000000003", "big.txt", "text", "text/plain", "hosted", []byte(body), nil)

	res, err := fetch(t, f, "/files/download?file=F0000000003", 4096)
	require.NoError(t, err)
	txt := res.Text
	assert.True(t, txt.Truncated)
	assert.Positive(t, txt.NextOffset)
	assert.Len(t, txt.Lines, txt.NextOffset)
	raw, err := json.Marshal(txt)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(raw), 4096-textSlack, "the answer fits its budget whole, the item around it included")
}

// Every refusal before the download: nothing about the file is fetched, and a
// file that is not public is described by its id alone.
func TestFetch_RefusedBeforeDownload(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		mimetype, mode string
		edit           func(map[string]any)
		code           string
	}{
		"private":             {"text/csv", "snippet", func(m map[string]any) { m["is_public"] = false }, CodeNotPublic},
		"no public channel":   {"text/csv", "snippet", func(m map[string]any) { m["channels"] = []string{}; m["groups"] = []string{"G0123456789"} }, CodeNotPublic},
		"external":            {"text/csv", "external", func(m map[string]any) { m["is_external"] = true }, CodeUnsupported},
		"tombstone":           {"text/csv", "tombstone", nil, CodeUnsupported},
		"no download link":    {"text/csv", "hosted", func(m map[string]any) { delete(m, "url_private_download") }, CodeUnsupported},
		"too large":           {"text/csv", "hosted", func(m map[string]any) { m["size"] = maxFileBytes + 1 }, CodeTooLarge},
		"large archive":       {"application/zip", "hosted", func(m map[string]any) { m["size"] = maxFileBytes + 1 }, CodeUnsupported},
		"spreadsheet":         {"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "hosted", nil, CodeUnsupported},
		"archive":             {"application/zip", "hosted", nil, CodeUnsupported},
		"pdf":                 {"application/pdf", "hosted", nil, CodeUnsupported},
		"image, masking mode": {"image/png", "hosted", nil, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := filestest.NewSlack(t)
			f.Add("F0000000004", "secret-plan.csv", "csv", tc.mimetype, tc.mode, []byte("a;b\n"), tc.edit)
			client := upstream.New(upstream.Options{Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20})
			mode := "warn"
			if tc.code == "" {
				mode, tc.code = "mask", CodeUnsupported
			}
			_, err := NewFetcher(client).Fetch(context.Background(), profileFor(t, f, mode), "/files/download?file=F0000000004", 32<<10)
			require.Error(t, err)
			assert.Equal(t, tc.code, refusalCode(err), err.Error())
			assert.Zero(t, f.Downloads(), "nothing is downloaded for a refused file")
			if tc.code == CodeNotPublic {
				assert.NotContains(t, err.Error(), "secret-plan", "a file that is not public is named by its id alone")
				assert.NotContains(t, err.Error(), "csv")
			}
		})
	}
}

// A file shared to a public channel only through shares.public still counts.
func TestFetch_PublicThroughShares(t *testing.T) {
	t.Parallel()
	f := filestest.NewSlack(t)
	f.Add("F0000000005", "notes.txt", "text", "text/plain", "snippet", []byte("hello\n"), func(m map[string]any) {
		m["channels"] = []string{}
		m["shares"] = map[string]any{"public": map[string]any{"C0000000009": []any{}}, "private": map[string]any{"G0000000001": []any{}}}
	})
	res, err := fetch(t, f, "/files/download?file=F0000000005", 32<<10)
	require.NoError(t, err)
	assert.Equal(t, []string{"C0000000009"}, res.Meta.Channels, "only public channels are named")
}

func TestFetch_UpstreamFailures(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		edit   func(*filestest.Slack)
		want   string
		status int
	}{
		"unknown file": {func(*filestest.Slack) {}, "file_not_found", http.StatusOK},
		"another file": {func(f *filestest.Slack) {
			f.Add("F0000000006", "a.txt", "text", "text/plain", "hosted", []byte("a\n"), func(m map[string]any) { m["id"] = "F0000000007" })
		}, "answered for", http.StatusOK},
		"size mismatch": {func(f *filestest.Slack) {
			f.Add("F0000000006", "a.txt", "text", "text/plain", "hosted", []byte("a\n"), func(m map[string]any) { m["size"] = 99 })
		}, "download_mismatch", http.StatusOK},
		"sign-in page": {func(f *filestest.Slack) {
			page := []byte("<html>sign in</html>")
			f.Add("F0000000006", "a.csv", "csv", "text/csv", "snippet", page, nil)
			f.Edit("F0000000006", func(ff *filestest.File) { ff.ContentType = "text/html; charset=utf-8" })
		}, "HTML page", http.StatusOK},
		"redirect": {func(f *filestest.Slack) {
			f.Add("F0000000006", "a.txt", "text", "text/plain", "hosted", []byte("a\n"), nil)
			f.Edit("F0000000006", func(ff *filestest.File) { ff.Status = http.StatusFound })
		}, "redirect", http.StatusFound},
		"foreign download host": {func(f *filestest.Slack) {
			f.Add("F0000000006", "a.txt", "text", "text/plain", "hosted", []byte("a\n"), func(m map[string]any) {
				m["url_private_download"] = "https://evil.example.com/files-pri/T0-F0000000006/a.txt"
			})
		}, "not on the target's host", 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := filestest.NewSlack(t)
			tc.edit(f)
			_, err := fetch(t, f, "/files/download?file=F0000000006", 32<<10)
			require.Error(t, err)
			assert.Empty(t, refusalCode(err), "an upstream failure, not a refusal")
			assert.Contains(t, err.Error(), tc.want)
			var up *UpstreamError
			if tc.status == 0 {
				assert.NotErrorAs(t, err, &up, "a URL refused before any request has no answer to speak of")
				return
			}
			require.ErrorAs(t, err, &up, "the upstream answered: its status travels with the error")
			assert.Equal(t, tc.status, up.Status)
		})
	}
}

// Slack keeps the file's name in the download URL: "..", as a part of a name,
// is not a step up the path.
func TestFetch_DotsInName(t *testing.T) {
	t.Parallel()
	f := filestest.NewSlack(t)
	f.Add("F0000000007", "report..csv", "csv", "text/csv", "snippet", []byte("a;b\n"), nil)
	res, err := fetch(t, f, "/files/download?file=F0000000007", 32<<10)
	require.NoError(t, err)
	assert.Equal(t, []string{"1: a;b"}, res.Text.Lines)
}

// An HTML report Slack did not recognise comes as binary; its name says it is
// text. An archive in the same disguise is refused by its name.
func TestFetch_UnknownTypeByName(t *testing.T) {
	t.Parallel()
	f := filestest.NewSlack(t)
	f.Add("F0000000008", "report.html", "binary", "application/octet-stream", "hosted", []byte("<p>ok</p>\n"), nil)
	f.Add("F0000000009", "data.rar", "binary", "application/octet-stream", "hosted", []byte("Rar!\x1a\x07\x00"), nil)

	res, err := fetch(t, f, "/files/download?file=F0000000008", 32<<10)
	require.NoError(t, err)
	assert.Equal(t, []string{"1: <p>ok</p>"}, res.Text.Lines)

	_, err = fetch(t, f, "/files/download?file=F0000000009", 32<<10)
	assert.Equal(t, CodeUnsupported, refusalCode(err))
	assert.Equal(t, 1, f.Downloads(), "the archive is refused before its download")
}

// A compressed download is measured unpacked, which is what files.info counts.
func TestFetch_GzipDownload(t *testing.T) {
	t.Parallel()
	f := filestest.NewSlack(t)
	f.Add("F000000000A", "app.log", "text", "text/plain", "hosted", []byte(strings.Repeat("same line\n", 500)), nil)
	f.Edit("F000000000A", func(ff *filestest.File) { ff.Gzip = true })
	res, err := fetch(t, f, "/files/download?file=F000000000A&limit=2", 32<<10)
	require.NoError(t, err)
	assert.Equal(t, 500, res.Text.TotalLines)
	assert.Equal(t, []string{"1: same line", "2: same line"}, res.Text.Lines)
}

func TestParseParams(t *testing.T) {
	t.Parallel()
	p, err := parseParams("/files/download?file=F0123456789&offset=5&limit=10&grep=a%3Bb")
	require.NoError(t, err)
	assert.Equal(t, params{File: "F0123456789", Offset: 5, Limit: 10, Grep: "a;b"}, p)

	p, err = parseParams("/files/download?file=F0123456789")
	require.NoError(t, err)
	assert.Equal(t, defaultLimit, p.Limit)

	for _, bad := range []string{
		"/files/download",
		"/files/download?file=https%3A%2F%2Fevil.example.com%2Fa",
		"/files/download?file=f0123456789",
		"/files/download?file=F0123456789&file=F0000000001",
		"/files/download?file=F0123456789&limit=0",
		"/files/download?file=F0123456789&limit=1001",
		"/files/download?file=F0123456789&offset=-1",
		"/files/download?file=F0123456789&grep=",
		"/files/download?file=F0123456789&channel=C0123456789",
	} {
		_, err := parseParams(bad)
		assert.ErrorIs(t, err, ErrBadParams, bad)
	}
}
