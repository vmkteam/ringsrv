package rpc

import (
	"encoding/base64"
	"image/color"
	"net/http"
	"testing"
	"time"

	"github.com/vmkteam/ringsrv/pkg/client/upstream"
	"github.com/vmkteam/ringsrv/pkg/ring"
	"github.com/vmkteam/ringsrv/pkg/ring/files"
	"github.com/vmkteam/ringsrv/pkg/ring/files/filestest"
	"github.com/vmkteam/ringsrv/pkg/ring/target"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmkteam/embedlog"
	"github.com/vmkteam/mcpkit/audit"
	"github.com/vmkteam/mcpkit/mcp"
)

// filesService builds a service whose chat target serves the file route. Its
// DefaultJQ would turn any answer into null: the file route must not run it.
func filesService(t *testing.T, baseURL string, l embedlog.Logger) ToolsService {
	t.Helper()
	return filesServiceRedacting(t, baseURL, l, "warn")
}

// filesServiceRedacting is filesService with the target's redaction mode.
func filesServiceRedacting(t *testing.T, baseURL string, l embedlog.Logger, redactMode string) ToolsService {
	t.Helper()
	toml := `
Env = "dev"

[Profiles.chat]
Description      = "dev · chat"
BaseURL          = "` + baseURL + `"
Headers          = ["Authorization: Bearer chat-token"]
AllowMethods     = ["GET"]
AllowPaths       = ["^/api/conversations\\.replies$", "^/files/download$"]
AllowQueryParams = ["channel", "ts", "file", "offset", "limit<=1000", "grep", "sheet", "entry"]
DefaultJQ        = ".nothing"
Files            = "slack"
Redact           = "` + redactMode + `"
RedactRules      = ["email"]

[Roles.viewer]
Description = "read"
Groups      = ["ringsrv-users"]
Tools       = ["api_call"]
Targets     = ["chat"]
`
	cat, err := target.Parse([]byte(toml))
	require.NoError(t, err)
	return NewToolsService(ToolsDeps{
		Targets:   cat,
		Upstream:  upstream.New(upstream.Options{Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20}),
		MaxBytes:  32 << 10,
		JQTimeout: time.Second,
		Sessions:  ring.NewSessions(time.Hour, nil),
		Logger:    l,
		Docs:      testDocs(t),
	})
}

func fileCall(path string) map[string]any {
	return map[string]any{"target": "chat", "method": "GET", "path": path}
}

const chatReport = "code;owner\n809000001;ann@example.com\n809000002;bob\n"

func TestFiles_TextThroughAPICall(t *testing.T) {
	t.Parallel()
	chat := filestest.NewSlack(t)
	chat.Add("F0000000001", "report.csv", "csv", "text/csv", "snippet", []byte(chatReport), nil)
	s := filesService(t, chat.URL(), embedlog.Logger{})

	res, err := s.Call(ctxWithGroups("ringsrv-users"), ToolAPICall, apiArgs(fileCall("/files/download?file=F0000000001&grep=809000001")))
	require.NoError(t, err)
	require.Len(t, res.Content, 1, "no picture, no block")
	item := decodeOK(t, res)
	assert.False(t, item.DefaultJQ, "the route's own JSON is not run through the profile's filter")
	data, ok := item.Data.(map[string]any)
	require.True(t, ok, "%#v", item.Data)
	assert.Equal(t, []any{"2: 809000001;ann@example.com"}, data["lines"])
	assert.Equal(t, "code;owner", data["header"])
	assert.Equal(t, []string{"email"}, item.Redacted, "the target's PII rules read the file like any answer")

	// A jq the call brings does run.
	call := fileCall("/files/download?file=F0000000001")
	call["jq"] = ".total_lines"
	res, err = s.Call(ctxWithGroups("ringsrv-users"), ToolAPICall, apiArgs(call))
	require.NoError(t, err)
	assert.EqualValues(t, 3, decodeOK(t, res).Data)
}

func TestFiles_ImageBlock(t *testing.T) {
	t.Parallel()
	pic := filestest.PNG(t, 16, 16, color.RGBA{R: 255, A: 255})
	chat := filestest.NewSlack(t)
	chat.Add("F0000000002", "image.png", "png", "image/png", "hosted", pic, nil)
	s := filesService(t, chat.URL(), embedlog.Logger{})

	// The same call twice: one picture, two items pointing at it.
	res, err := s.Call(ctxWithGroups("ringsrv-users"), ToolAPICall, map[string]any{"calls": []any{
		fileCall("/files/download?file=F0000000002"),
		fileCall("/files/download?file=F0000000002"),
	}})
	require.NoError(t, err)
	require.Len(t, res.Content, 2, "the JSON and one picture")
	block := res.Content[1]
	assert.Equal(t, mcp.ContentTypeImage, block.Type)
	assert.Equal(t, "image/png", block.MimeType)
	raw, err := base64.StdEncoding.DecodeString(block.Data)
	require.NoError(t, err)
	assert.Equal(t, pic, raw)

	out := decodeBatch(t, res)
	require.Len(t, out.Results, 2)
	first := out.Results[0]
	require.NotNil(t, first.Image)
	assert.Equal(t, ImageRef{Content: 1, MimeType: "image/png", Width: 16, Height: 16, Bytes: len(pic)}, *first.Image)
	data, ok := first.Data.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "image.png", data["name"], "the JSON describes the file")
	require.NotNil(t, out.Results[1].DedupOf)
	assert.Equal(t, first.Image, out.Results[1].Image, "the repeat points at the same block")
}

// Past the ceiling a picture is dropped with a reason, the rest still arrive.
func TestFiles_ImageCeiling(t *testing.T) {
	t.Parallel()
	chat := filestest.NewSlack(t)
	calls := make([]any, 0, 5)
	for i, id := range []string{"F000000000A", "F000000000B", "F000000000C", "F000000000D", "F000000000E"} {
		chat.Add(id, id+".png", "png", "image/png", "hosted", filestest.PNG(t, 16, 16, color.RGBA{R: uint8(i * 40), A: 255}), nil)
		calls = append(calls, fileCall("/files/download?file="+id))
	}
	s := filesService(t, chat.URL(), embedlog.Logger{})

	res, err := s.Call(ctxWithGroups("ringsrv-users"), ToolAPICall, map[string]any{"calls": calls})
	require.NoError(t, err)
	assert.Len(t, res.Content, 1+maxImagesPerCall)
	out := decodeBatch(t, res)
	for i, item := range out.Results[:maxImagesPerCall] {
		require.NotNil(t, item.Image, i)
		assert.Equal(t, i+1, item.Image.Content)
	}
	last := out.Results[maxImagesPerCall]
	require.NotNil(t, last.Error)
	assert.Equal(t, ErrCodeTruncated, last.Error.Code)
	assert.Nil(t, last.Image)
	assert.True(t, out.Truncated)
}

// A file outside public channels is refused with its own code, and the refusal
// is in the log like any other.
func TestFiles_NotPublicAudited(t *testing.T) {
	chat := filestest.NewSlack(t)
	chat.Add("F0000000003", "salaries.csv", "csv", "text/csv", "snippet", []byte("a\n"), func(info map[string]any) { info["is_public"] = false })
	chat.Add("F0000000004", "notes.txt", "text", "text/plain", "snippet", []byte("a\n"), nil)
	var res mcp.ToolCallResult
	recs := captureLog(t, func(l embedlog.Logger) {
		var err error
		res, err = filesService(t, chat.URL(), l).Call(ctxWithGroups("ringsrv-users"), ToolAPICall, map[string]any{"calls": []any{
			fileCall("/files/download?file=F0000000003"),
			fileCall("/files/download?file=F0000000004"),
		}})
		require.NoError(t, err)
	})
	out := decodeBatch(t, res)
	require.NotNil(t, out.Results[0].Error)
	assert.Equal(t, ErrCodeFileNotPublic, out.Results[0].Error.Code)
	assert.NotContains(t, res.Content[0].Text, "salaries")

	records := itemRecords(recs)
	require.Len(t, records, 2)
	byIndex := map[float64]map[string]any{}
	for _, r := range records {
		idx, ok := r["batch_index"].(float64)
		require.True(t, ok)
		byIndex[idx] = r
	}
	assert.Equal(t, audit.DecisionDeny, byIndex[0]["decision"])
	assert.Equal(t, ErrCodeFileNotPublic, byIndex[0]["deny_reason"])
	assert.NotContains(t, byIndex[0], "file_name", "a refused file leaves no name in the log")

	assert.Equal(t, audit.DecisionAllow, byIndex[1]["decision"])
	assert.Equal(t, "notes.txt", byIndex[1]["file_name"])
	assert.Equal(t, "C0123456789", byIndex[1]["file_channels"])
	assert.Equal(t, false, byIndex[1]["image"])
}

// A workbook inside an archive comes as the lines of a sheet, its PII masked
// like any text, and the audit names the file inside as well as the archive.
func TestFiles_ArchiveEntryAudited(t *testing.T) {
	t.Parallel()
	book := filestest.XLSX(t, filestest.Workbook{
		Shared: []string{"code", "owner", "ann@example.com"},
		Sheets: []filestest.Sheet{{Name: "Лист1", Rows: `<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c></row>` +
			`<row r="2"><c r="A2"><v>809000001</v></c><c r="B2" t="s"><v>2</v></c></row>`}},
	})
	chat := filestest.NewSlack(t)
	chat.Add("F0000000005", "export.zip", "zip", "application/zip", "hosted", filestest.Zip(t, filestest.Entry{Name: "owners.xlsx", Body: book}), nil)

	var res mcp.ToolCallResult
	recs := captureLog(t, func(l embedlog.Logger) {
		var err error
		res, err = filesService(t, chat.URL(), l).Call(ctxWithGroups("ringsrv-users"), ToolAPICall,
			apiArgs(fileCall("/files/download?file=F0000000005&entry=owners.xlsx")))
		require.NoError(t, err)
	})
	item := decodeOK(t, res)
	data, ok := item.Data.(map[string]any)
	require.True(t, ok, "%#v", item.Data)
	assert.Equal(t, []any{"1: code\towner", "2: 809000001\tann@example.com"}, data["lines"])
	assert.Equal(t, "Лист1", data["sheet"])
	assert.Equal(t, "owners.xlsx", data["entry"])
	assert.Equal(t, []string{"email"}, item.Redacted)

	records := itemRecords(recs)
	require.Len(t, records, 1)
	assert.Equal(t, "export.zip", records[0]["file_name"])
	assert.Equal(t, "owners.xlsx", records[0]["file_entry"])
}

// The picture ceiling counts bytes as well as pictures: past 3 MB of them the
// next one is dropped, however few came before.
func TestAttachImages_ByteCeiling(t *testing.T) {
	t.Parallel()
	item := func(i int) APICallItem {
		return APICallItem{Index: i, Data: map[string]any{"id": i}, img: &files.Image{MimeType: "image/png", Data: make([]byte, 1200<<10)}}
	}
	items := []APICallItem{item(0), item(1), item(2)}
	blocks := attachImages(items)
	assert.Len(t, blocks, 2, "two of 1.2 MB fit, a third would make 3.6")
	assert.Equal(t, 2, items[1].Image.Content)
	require.NotNil(t, items[2].Error)
	assert.Equal(t, ErrCodeTruncated, items[2].Error.Code)
	assert.Nil(t, items[2].Data)
	assert.Nil(t, items[2].Image)
}

// A file the upstream does not know is its answer, not a failure of this
// server: the item says so with the status, and nothing is logged as an error.
func TestFiles_UnknownFileIsNotAnError(t *testing.T) {
	chat := filestest.NewSlack(t)
	var res mcp.ToolCallResult
	recs := captureLog(t, func(l embedlog.Logger) {
		var err error
		res, err = filesService(t, chat.URL(), l).Call(ctxWithGroups("ringsrv-users"), ToolAPICall,
			apiArgs(fileCall("/files/download?file=F0000000404")))
		require.NoError(t, err)
	})
	item := decodeBatch(t, res).Results[0]
	require.NotNil(t, item.Error)
	assert.Equal(t, ErrCodeUpstream, item.Error.Code)
	assert.Contains(t, item.Error.Message, "file_not_found")
	assert.Equal(t, http.StatusOK, item.Status)
	for _, r := range recs {
		assert.NotEqual(t, "upstream failed", r["msg"], "an upstream that answered is not logged as a failure")
	}
}

// A jq that fails over the route's own JSON says which keys that JSON has.
func TestFiles_JQFailure(t *testing.T) {
	t.Parallel()
	chat := filestest.NewSlack(t)
	chat.Add("F0000000001", "report.csv", "csv", "text/csv", "snippet", []byte(chatReport), nil)
	call := fileCall("/files/download?file=F0000000001")
	call["jq"] = ".lines | .foo"
	res, err := filesService(t, chat.URL(), embedlog.Logger{}).Call(ctxWithGroups("ringsrv-users"), ToolAPICall, apiArgs(call))
	require.NoError(t, err)
	item := decodeBatch(t, res).Results[0]
	require.NotNil(t, item.Error)
	assert.Equal(t, ErrCodeJQFailed, item.Error.Code)
	assert.Contains(t, item.Error.Keys, "lines")
}

// On a target that masks, the file's text is masked like any answer.
func TestFiles_TextMasked(t *testing.T) {
	t.Parallel()
	chat := filestest.NewSlack(t)
	chat.Add("F0000000001", "report.csv", "csv", "text/csv", "snippet", []byte(chatReport), nil)
	s := filesServiceRedacting(t, chat.URL(), embedlog.Logger{}, "mask")
	res, err := s.Call(ctxWithGroups("ringsrv-users"), ToolAPICall, apiArgs(fileCall("/files/download?file=F0000000001&grep=809000001")))
	require.NoError(t, err)
	item := decodeOK(t, res)
	assert.NotContains(t, res.Content[0].Text, "ann@example.com")
	assert.Equal(t, []string{"email"}, item.Redacted)
}

// The file route's sheet is listed for a role that has a target serving it,
// and read like any other.
func TestFiles_HelpSheet(t *testing.T) {
	t.Parallel()
	chat := filestest.NewSlack(t)
	s := filesService(t, chat.URL(), embedlog.Logger{})

	res, err := s.Call(ctxWithGroups("ringsrv-users"), ToolHelp, nil)
	require.NoError(t, err)
	assert.Contains(t, sheetNamesOf(decodeHelp(t, res)), "files")

	res, err = s.Call(ctxWithGroups("ringsrv-users"), ToolHelp, map[string]any{"names": []any{"files"}})
	require.NoError(t, err)
	out := decodeHelp(t, res)
	require.Len(t, out.Results, 1)
	require.Nil(t, out.Results[0].Error)
	assert.Contains(t, out.Results[0].Text, "/files/download")
	assert.Contains(t, out.Results[0].Text, "UnsupportedFileType")
}
