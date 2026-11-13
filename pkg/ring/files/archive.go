package files

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/vmkteam/mcpkit/redact"
	"golang.org/x/text/encoding/charmap"
)

// contentsHeader names the columns of an archive's table of contents.
const contentsHeader = "size\tmodified\tname"

// contents answers an archive with its table of contents: a line per file,
// its size unpacked, when it was changed and its name — the name entry takes.
// Folders and the __MACOSX shadows of an archive made on a Mac are left out:
// there is nothing in them to read.
func (f *Fetcher) contents(src source, p params, budget int) (*Result, error) {
	zr, err := openZip(src)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	for _, zf := range zr.File {
		name := entryName(zf)
		if skippedEntry(name) {
			continue
		}
		fmt.Fprintf(&buf, "%d\t%s\t%s\n", zf.UncompressedSize64, zf.Modified.UTC().Format("2006-01-02 15:04"), oneLine(name))
	}
	out := &Text{Meta: src.meta, Header: contentsHeader}
	out.fill(buf.Bytes(), p, budget, false)
	return &Result{Meta: src.meta, Text: out}, nil
}

// openEntry takes one file out of an archive, by the name its table of
// contents gives, and hands it on to be shown by its own kind. The size the
// archive declares is checked before unpacking and the unpacking is cut at
// the limit anyway: the declaration is the archive's word, and an archive
// that lies about it is how a small download becomes gigabytes.
func openEntry(src source, entry, redactMode string) (source, error) {
	zr, err := openZip(src)
	if err != nil {
		return source{}, err
	}
	var zf *zip.File
	for _, candidate := range zr.File {
		// The table of contents shows a name on one line; either form finds it.
		if name := entryName(candidate); (name == entry || oneLine(name) == entry) && !skippedEntry(name) {
			zf = candidate
			break
		}
	}
	id := src.meta.ID
	if zf == nil {
		return source{}, fmt.Errorf("%w: archive %s has no file %q — its table of contents is the answer without entry", ErrBadParams, id, entry)
	}

	meta := src.meta
	meta.Entry = entry
	kind := kindByName(entry)
	switch {
	case kind == kindArchive:
		return source{}, refused(CodeUnsupported, "%q in archive %s is an archive itself, and archives inside archives are not opened", entry, id)
	case kind == "":
		return source{}, refused(CodeUnsupported, "%q in archive %s is not something this server shows: only %s are", entry, id, kindNames)
	case kind == kindImage && redactMode == redact.ModeOn:
		return source{}, pictureMasked(id + " " + entry)
	case zf.Flags&0x1 != 0:
		return source{}, refused(CodeUnsupported, "%q in archive %s is encrypted", entry, id)
	case zf.UncompressedSize64 > maxFileBytes:
		return source{}, refused(CodeTooLarge, "%q in archive %s is %d bytes unpacked, over the %d this server reads", entry, id, zf.UncompressedSize64, maxFileBytes)
	}
	data, err := unpack(zf, maxFileBytes)
	switch {
	case errors.Is(err, errUnpackedTooLarge):
		return source{}, refused(CodeTooLarge, "%q in archive %s unpacks to more than the %d bytes this server reads", entry, id, maxFileBytes)
	case err != nil:
		return source{}, refused(CodeUnsupported, "%q in archive %s does not unpack: %s", entry, id, err)
	}
	return source{meta: meta, kind: kind, data: data}, nil
}

func openZip(src source) (*zip.Reader, error) {
	zr, err := zip.NewReader(bytes.NewReader(src.data), int64(len(src.data)))
	if err != nil {
		return nil, refused(CodeUnsupported, "file %s is not a zip archive this server can read: %s", src.meta.ID, err)
	}
	return zr, nil
}

var errUnpackedTooLarge = errors.New("unpacks past the limit")

// unpack reads a file of an archive whole, stopping past limit bytes whatever
// the archive declared.
func unpack(zf *zip.File, limit int64) ([]byte, error) {
	rc, err := zf.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errUnpackedTooLarge
	}
	return data, nil
}

// entryName is a file's name in an archive as UTF-8. A name the archive does
// not mark as UTF-8 and that is not valid UTF-8 is CP866: that is what the
// archiver built into Windows writes for Cyrillic names. A valid UTF-8 name
// without the mark — the archiver of macOS writes those, and Go reports them
// as NonUTF8 too — stays as it is.
func entryName(zf *zip.File) string {
	if !zf.NonUTF8 || utf8.ValidString(zf.Name) {
		return zf.Name
	}
	name, err := charmap.CodePage866.NewDecoder().String(zf.Name)
	if err != nil {
		return zf.Name
	}
	return name
}

// skippedEntry is a name that is not a file to read: a folder, or what an
// archive made on a Mac carries beside each file.
func skippedEntry(name string) bool {
	return strings.HasSuffix(name, "/") || strings.HasPrefix(name, "__MACOSX/")
}

// oneLine keeps a name from breaking the table of contents: a tab or a newline
// in it would read as a column or a file of its own.
func oneLine(s string) string {
	return strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(s)
}
