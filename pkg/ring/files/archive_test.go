package files

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"hash/crc32"
	"image/color"
	"strings"
	"testing"
	"time"

	"github.com/vmkteam/ringsrv/pkg/client/upstream"
	"github.com/vmkteam/ringsrv/pkg/ring/files/filestest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func exportZip(t *testing.T) []byte {
	t.Helper()
	return filestest.Zip(t,
		filestest.Entry{Name: "export/"},
		filestest.Entry{Name: "export/остатки.csv", Body: []byte("code;qty\n809000001;5\n809000002;0\n"), CP866: true},
		filestest.Entry{Name: "export/prices.xlsx", Body: prices(t)},
		filestest.Entry{Name: "export/shot.png", Body: filestest.PNG(t, 4, 3, color.White)},
		filestest.Entry{Name: "export/inner.zip", Body: filestest.Zip(t, filestest.Entry{Name: "a.txt", Body: []byte("a")})},
		filestest.Entry{Name: "export/report.pdf", Body: []byte("%PDF-1.4")},
		filestest.Entry{Name: "__MACOSX/export/._prices.xlsx", Body: []byte("junk")},
	)
}

func TestArchive_Contents(t *testing.T) {
	t.Parallel()
	f := filestest.NewSlack(t)
	f.Add("F0000000201", "export.zip", "zip", "application/zip", "hosted", exportZip(t), nil)

	res, err := fetch(t, f, "/files/download?file=F0000000201", 32<<10)
	require.NoError(t, err)
	require.NotNil(t, res.Text)
	txt := res.Text
	assert.Equal(t, contentsHeader, txt.Header, "the table of contents always names its columns")
	require.Len(t, txt.Lines, 5, "folders and the __MACOSX shadows are left out")
	assert.Equal(t, "1: 33\t2026-11-01 10:30\texport/остатки.csv", txt.Lines[0], "a CP866 name is read as Cyrillic")
	assert.True(t, strings.HasSuffix(txt.Lines[1], "\texport/prices.xlsx"))

	res, err = fetch(t, f, "/files/download?file=F0000000201&grep=xlsx", 32<<10)
	require.NoError(t, err)
	assert.Len(t, res.Text.Lines, 1, "grep finds a file in the table of contents")
}

func TestArchive_Entry(t *testing.T) {
	t.Parallel()
	f := filestest.NewSlack(t)
	f.Add("F0000000202", "export.zip", "binary", "application/octet-stream", "hosted", exportZip(t), nil)

	res, err := fetch(t, f, "/files/download?file=F0000000202&entry=export%2F%D0%BE%D1%81%D1%82%D0%B0%D1%82%D0%BA%D0%B8.csv&offset=1", 32<<10)
	require.NoError(t, err)
	assert.Equal(t, "export/остатки.csv", res.Meta.Entry)
	assert.Equal(t, "export/остатки.csv", res.Text.Entry)
	assert.Equal(t, "code;qty", res.Text.Header, "a table inside an archive keeps its column names too")
	assert.Equal(t, []string{"2: 809000001;5", "3: 809000002;0"}, res.Text.Lines)

	res, err = fetch(t, f, "/files/download?file=F0000000202&entry=export%2Fprices.xlsx&sheet=2&limit=2", 32<<10)
	require.NoError(t, err)
	assert.Equal(t, "Прайс", res.Text.Sheet, "a workbook inside an archive takes sheet")
	assert.Equal(t, []string{"1: code\tname\tprice\tuntil", "2: 809000001\tАспирин\t125.5\t2027-01-01"}, res.Text.Lines)

	res, err = fetch(t, f, "/files/download?file=F0000000202&entry=export%2Fshot.png", 32<<10)
	require.NoError(t, err)
	require.NotNil(t, res.Image)
	assert.Equal(t, "image/png", res.Image.MimeType)
	assert.Equal(t, "export/shot.png", res.Meta.Entry)
}

func TestArchive_EntryRefused(t *testing.T) {
	t.Parallel()
	f := filestest.NewSlack(t)
	f.Add("F0000000203", "export.zip", "zip", "application/zip", "hosted", exportZip(t), nil)

	for entry, code := range map[string]string{
		"export%2Finner.zip":                CodeUnsupported,
		"export%2Freport.pdf":               CodeUnsupported,
		"__MACOSX%2Fexport%2F._prices.xlsx": "",
		"export%2F":                         "",
		"missing.csv":                       "",
	} {
		_, err := fetch(t, f, "/files/download?file=F0000000203&entry="+entry, 32<<10)
		require.Error(t, err, entry)
		if code == "" {
			require.ErrorIs(t, err, ErrBadParams, entry)
			continue
		}
		assert.Equal(t, code, refusalCode(err), entry)
	}

	_, err := fetch(t, f, "/files/download?file=F0000000203&sheet=1", 32<<10)
	require.ErrorIs(t, err, ErrBadParams, "sheet without entry on an archive")
	_, err = fetch(t, f, "/files/download?file=F0000000203&entry=export%2Fshot.png&sheet=1", 32<<10)
	require.ErrorIs(t, err, ErrBadParams, "sheet of a picture inside an archive")

	f.Add("F0000000204", "report.csv", "csv", "text/csv", "hosted", []byte("a;b\n"), nil)
	_, err = fetch(t, f, "/files/download?file=F0000000204&entry=a.csv", 32<<10)
	require.ErrorIs(t, err, ErrBadParams, "entry of a file that is not an archive")

	f.Add("F0000000205", "junk.zip", "zip", "application/zip", "hosted", []byte("not a zip"), nil)
	_, err = fetch(t, f, "/files/download?file=F0000000205", 32<<10)
	assert.Equal(t, CodeUnsupported, refusalCode(err))
}

// A picture inside an archive is refused on a target that masks, as a picture
// on its own is.
func TestArchive_PictureOnMaskingTarget(t *testing.T) {
	t.Parallel()
	f := filestest.NewSlack(t)
	f.Add("F0000000206", "export.zip", "zip", "application/zip", "hosted", exportZip(t), nil)
	client := upstream.New(upstream.Options{Timeout: 5 * time.Second, MaxBodyBytes: 1 << 20})
	fetcher := NewFetcher(client, nil)

	_, err := fetcher.Fetch(context.Background(), profileFor(t, f, "mask"), "/files/download?file=F0000000206&entry=export%2Fshot.png", 32<<10)
	assert.Equal(t, CodeUnsupported, refusalCode(err))
	assert.Contains(t, err.Error(), "masks personal data")

	_, err = fetcher.Fetch(context.Background(), profileFor(t, f, "mask"), "/files/download?file=F0000000206&entry=export%2Fprices.xlsx", 32<<10)
	require.NoError(t, err, "a workbook is text and is masked as text")
}

// What an archive says a file unpacks to is checked before unpacking, and the
// unpacking stops at the limit whatever the archive said.
func TestArchive_UnpackedSize(t *testing.T) {
	t.Parallel()
	big := bytes.Repeat([]byte("0123456789\n"), maxFileBytes/11+100)

	honest := filestest.Zip(t, filestest.Entry{Name: "big.txt", Body: big})
	lying := rawZip(t, "big.txt", big, 100)

	f := filestest.NewSlack(t)
	f.Add("F0000000207", "honest.zip", "zip", "application/zip", "hosted", honest, nil)
	f.Add("F0000000208", "lying.zip", "zip", "application/zip", "hosted", lying, nil)

	_, err := fetch(t, f, "/files/download?file=F0000000207&entry=big.txt", 32<<10)
	assert.Equal(t, CodeTooLarge, refusalCode(err), "declared over the limit")

	_, err = fetch(t, f, "/files/download?file=F0000000208&entry=big.txt", 32<<10)
	require.Error(t, err, "declared small, unpacks large")
	assert.NotEmpty(t, refusalCode(err))
}

// rawZip is an archive of one file whose header declares size bytes, whatever
// the data unpacks to.
func rawZip(t *testing.T, name string, data []byte, size uint64) []byte {
	t.Helper()
	var packed bytes.Buffer
	fw, err := flate.NewWriter(&packed, flate.BestCompression)
	require.NoError(t, err)
	_, err = fw.Write(data)
	require.NoError(t, err)
	require.NoError(t, fw.Close())

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateRaw(&zip.FileHeader{
		Name: name, Method: zip.Deflate, CRC32: crc32.ChecksumIEEE(data),
		CompressedSize64: uint64(packed.Len()), UncompressedSize64: size,
	})
	require.NoError(t, err)
	_, err = w.Write(packed.Bytes())
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}
