package files

import (
	"context"
	"maps"
	"path"
	"strings"

	"github.com/vmkteam/mcpkit/mcp"
	"github.com/vmkteam/mcpkit/redact"
)

// File kinds the route can show.
const (
	kindText    = "text"
	kindImage   = "image"
	kindSheet   = "sheet"
	kindArchive = "archive"
)

// source is a file a provider has found, checked and downloaded: what it is,
// how it is to be shown, and its bytes.
type source struct {
	meta Meta
	kind string
	data []byte
}

// MIME types of the kinds that are not text or pictures.
const (
	mimeXLSX = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	mimeXLSM = "application/vnd.ms-excel.sheet.macroenabled.12"
)

var zipMIMEs = map[string]bool{"application/zip": true, "application/x-zip-compressed": true, "application/x-zip": true}

// classify picks the kind of a file from the type its upstream gives and,
// when that says nothing, from its name. A type that names something else is
// believed: a PDF called report.csv is a PDF.
//
// Whatever the name, a NUL near the start still refuses a "text" file once the
// bytes are in, and a picture's format is read from its bytes.
func classify(name, mimetype string) string {
	mt := mediaType(mimetype)
	format, isImage := strings.CutPrefix(mt, "image/")
	switch {
	case mt == "application/x-yaml", mt != "" && mcp.IsTextualMIME(mt):
		return kindText
	case isImage && imageFormats[format]:
		return kindImage
	case mt == mimeXLSX, mt == mimeXLSM:
		return kindSheet
	case zipMIMEs[mt]:
		return kindArchive
	case mt == "" || mt == "application/octet-stream" || mt == "binary/octet-stream":
		return kindByName(name)
	}
	return ""
}

// kindByName is the kind a file's extension says, for a file whose type is
// unknown — the files inside an archive and the files of a store have nothing
// else to go by.
func kindByName(name string) string {
	ext := extension(name)
	switch {
	case textExtensions[ext]:
		return kindText
	case imageExtensions[ext]:
		return kindImage
	case ext == "xlsx", ext == "xlsm":
		return kindSheet
	case ext == "zip":
		return kindArchive
	}
	return ""
}

// textExtensions are the names of text files whose type is not known.
var textExtensions = map[string]bool{
	extCSV: true, extTSV: true, "txt": true, "log": true, "json": true, "yaml": true, "yml": true,
	"md": true, "html": true, "htm": true, "xml": true, "sql": true,
}

// imageExtensions are the names of the pictures: the formats, and jpg.
var imageExtensions = func() map[string]bool {
	m := maps.Clone(imageFormats)
	m["jpg"] = true
	return m
}()

// extension is a name's extension without the dot, lower case.
func extension(name string) string {
	return strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))
}

// mediaType is a Content-Type without its parameters, lower case.
func mediaType(contentType string) string {
	mt, _, _ := strings.Cut(strings.ToLower(contentType), ";")
	return strings.TrimSpace(mt)
}

// kindNames lists what the route shows, in a refusal.
const kindNames = "text, " + imageKinds + " images, xlsx workbooks and zip archives"

// admit is the check every provider makes before a byte is downloaded, once
// the file is known to be public: something this route can show, which a
// masking target may carry, of a size it downloads. What the file is comes
// before how big it is: a PDF is refused as one, not as too large, which
// would read as "a smaller one would do".
func admit(meta Meta, kind, mimetype, redactMode string) error {
	switch {
	case kind == "":
		return refused(CodeUnsupported, "file %s is %s (%s): only %s are served", meta.ID, meta.FileType, mimetype, kindNames)
	case kind == kindImage && redactMode == redact.ModeOn:
		return pictureMasked(meta.ID)
	case meta.Size > maxFileBytes:
		return refused(CodeTooLarge, "file %s is %d bytes, over the %d this server downloads", meta.ID, meta.Size, maxFileBytes)
	}
	return nil
}

// pictureMasked refuses a picture on a target that promises masking: a
// picture cannot be masked.
func pictureMasked(id string) error {
	return refused(CodeUnsupported, "file %s is an image, and this target masks personal data, which a picture does not allow", id)
}

// render shows a downloaded file the way its kind asks: text as numbered
// lines, a picture as an image, a workbook as the lines of one sheet, and an
// archive as its table of contents or, with entry, as one of its files shown
// by the same rules — except an archive inside it, which is not opened.
func (f *Fetcher) render(ctx context.Context, src source, p params, budget int, redactMode string) (*Result, error) {
	if err := p.fits(src.kind, src.meta.Entry != ""); err != nil {
		return nil, err
	}
	switch src.kind {
	case kindArchive:
		if p.Entry == "" {
			return f.contents(src, p, budget)
		}
		inner, err := openEntry(src, p.Entry, redactMode)
		if err != nil {
			return nil, err
		}
		return f.render(ctx, inner, p, budget, redactMode)
	case kindImage:
		img, err := f.picture(ctx, src.data)
		if err != nil {
			return nil, err
		}
		return &Result{Meta: src.meta, Image: img}, nil
	case kindSheet:
		text, err := f.sheet(ctx, src, p, budget)
		if err != nil {
			return nil, err
		}
		return &Result{Meta: src.meta, Text: text}, nil
	default:
		text, err := textOf(src.meta, src.data, p, budget, isTable(src.meta))
		if err != nil {
			return nil, err
		}
		return &Result{Meta: src.meta, Text: text}, nil
	}
}

// isTable says the file is a table whose first line names its columns: by
// the type its upstream gave, or by the name of a file inside an archive.
func isTable(meta Meta) bool {
	name := meta.Name
	if meta.Entry != "" {
		name = meta.Entry
	} else if meta.FileType == extCSV || meta.FileType == extTSV {
		return true
	}
	ext := extension(name)
	return ext == extCSV || ext == extTSV
}

// The tables among the text files.
const (
	extCSV = "csv"
	extTSV = "tsv"
)
