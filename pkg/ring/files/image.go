package files

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	_ "image/gif" // registers the decoder: the first frame of an animation is what can be shown
	"image/jpeg"
	"image/png"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // registers the decoder
)

// Sizes of a served picture. The long side matches what a model sees anyway:
// a larger picture is scaled down on its side, after costing the bytes here.
const (
	maxImageSide   = 1568
	smallImageSide = 1024
	// maxImageBytes is the most one picture may weigh after encoding.
	maxImageBytes = 1 << 20
	// maxImagePixels refuses a picture before decoding it: a small file can
	// declare a huge canvas, and decoding allocates all of it.
	maxImagePixels = 24_000_000
	// maxDecodeBytes is what one decode may allocate, by decodeCost. With
	// maxDecodes at one and the scaling below allocating a fraction of it,
	// a picture costs the process well under half of the smallest contour.
	maxDecodeBytes = 64 << 20
	jpegQuality    = 80
)

// imageFormats are the pictures an image block may carry, by the name the
// image package reports; the imports register their decoders. The MIME type
// of each is "image/" and the name.
var imageFormats = map[string]bool{"png": true, "jpeg": true, "gif": true, "webp": true}

// imageKinds names imageFormats in a refusal.
const imageKinds = "png, jpeg, gif and webp"

// Picture types this route encodes into.
const (
	mimePNG  = "image/png"
	mimeJPEG = "image/jpeg"
)

// picture prepares a picture for an image block. The format is read from the
// bytes, not from what the upstream said the file is, and the whole picture is
// decoded even when it travels as it came: a broken one would be refused by
// whoever reads the block, and with it the turn that carried it. One that is
// small enough travels as it came; a larger one is scaled down and encoded
// again.
func (f *Fetcher) picture(ctx context.Context, data []byte) (*Image, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || !imageFormats[format] {
		return nil, refused(CodeUnsupported, "the file is not a %s picture", imageKinds)
	}
	if int64(cfg.Width)*int64(cfg.Height) > maxImagePixels {
		return nil, refused(CodeTooLarge, "the picture is %d×%d, over the %d pixels this server decodes", cfg.Width, cfg.Height, maxImagePixels)
	}
	if cost := decodeCost(cfg, format, data); cost > maxDecodeBytes {
		return nil, refused(CodeTooLarge, "the picture is %d×%d and takes %d MB to decode, over the %d MB this server spends on one",
			cfg.Width, cfg.Height, cost>>20, maxDecodeBytes>>20)
	}

	select {
	case f.decodes <- struct{}{}:
		defer func() { <-f.decodes }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, refused(CodeUnsupported, "the picture does not decode as %s: %s", format, err)
	}
	if max(cfg.Width, cfg.Height) <= maxImageSide && len(data) <= maxImageBytes {
		return &Image{MimeType: "image/" + format, Data: data, Width: cfg.Width, Height: cfg.Height}, nil
	}
	// A screenshot stays PNG, where text is sharp; a photo goes to JPEG, and so
	// does a PNG that does not fit.
	lossless := format == "png" || format == "gif"
	for _, side := range []int{maxImageSide, smallImageSide} {
		scaled := scale(src, side)
		if lossless {
			if img, err := encode(scaled, true); err != nil || img != nil {
				return img, err
			}
		}
		if img, err := encode(onWhite(scaled), false); err != nil || img != nil {
			return img, err
		}
	}
	return nil, refused(CodeTooLarge, "the picture does not fit %d bytes even at %d px", maxImageBytes, smallImageSide)
}

// decodeCost is what decoding the picture allocates, read from its header: the
// pixels at the size of its colour model, and for a JPEG what its frame header
// says the planes and the decoder take.
func decodeCost(cfg image.Config, format string, data []byte) int64 {
	px := int64(cfg.Width) * int64(cfg.Height)
	if format == "jpeg" {
		if frame, ok := readJPEGFrame(data); ok {
			return frame.cost(px)
		}
		return 16 * px // a frame header this server cannot read: the most a JPEG takes
	}
	switch cfg.ColorModel {
	case color.GrayModel, color.AlphaModel:
		return px
	case color.Gray16Model, color.Alpha16Model:
		return 2 * px
	case color.RGBA64Model, color.NRGBA64Model:
		return 8 * px
	}
	if _, ok := cfg.ColorModel.(color.Palette); ok {
		return px
	}
	return 4 * px
}

// jpegFrame is what the frame header of a JPEG says about decoding it.
type jpegFrame struct {
	components int
	// samples is the bytes a pixel takes over all the colour planes, the
	// subsampled ones counted at their size.
	samples     float64
	progressive bool
}

// cost is what decoding allocates: the planes; for four components one more
// picture of 4 bytes a pixel, which CMYK is converted into; and for a
// progressive JPEG every coefficient of the picture, 4 bytes a sample, held
// until its last scan.
func (f jpegFrame) cost(px int64) int64 {
	perPixel := f.samples
	if f.components == 4 {
		perPixel += 4
	}
	if f.progressive {
		perPixel += 4 * f.samples
	}
	return int64(perPixel * float64(px))
}

// readJPEGFrame walks the segments of a JPEG to its frame header — the ones
// Go decodes: baseline, extended and progressive.
func readJPEGFrame(data []byte) (jpegFrame, bool) {
	for i := 2; i+4 <= len(data); { // past the start-of-image marker
		if data[i] != 0xFF {
			return jpegFrame{}, false
		}
		marker := data[i+1]
		switch {
		case marker == 0xFF: // fill byte before a marker
			i++
			continue
		case marker == 0x01, marker >= 0xD0 && marker <= 0xD8: // markers without a length
			i += 2
			continue
		}
		size := int(data[i+2])<<8 | int(data[i+3])
		if marker == 0xC0 || marker == 0xC1 || marker == 0xC2 {
			frame, ok := parseJPEGFrame(data[i+4 : min(len(data), i+2+size)])
			frame.progressive = marker == 0xC2
			return frame, ok
		}
		i += 2 + size
	}
	return jpegFrame{}, false
}

// parseJPEGFrame reads a frame header after its length: precision, height,
// width, then per component an id, its sampling factors and a table.
func parseJPEGFrame(b []byte) (jpegFrame, bool) {
	if len(b) < 6 {
		return jpegFrame{}, false
	}
	n := int(b[5])
	if n == 0 || len(b) < 6+3*n {
		return jpegFrame{}, false
	}
	hmax, vmax, sum := 1, 1, 0
	for c := range n {
		h, v := int(b[6+3*c+1]>>4), int(b[6+3*c+1]&0x0F)
		if h == 0 || v == 0 {
			return jpegFrame{}, false
		}
		hmax, vmax, sum = max(hmax, h), max(vmax, v), sum+h*v
	}
	return jpegFrame{components: n, samples: float64(sum) / float64(hmax*vmax)}, true
}

// scale fits src into side. Halved first while it is twice the size or more —
// bilinear sampling at exactly half is the mean of each 2×2 square, for any
// kind of picture — then averaged down to the size. Neither step allocates
// beyond its result: the kernel scalers of x/image/draw keep a buffer of the
// result's width by the source's height in 32-byte cells, which for a 5K
// screenshot is more than the picture itself.
func scale(src image.Image, side int) *image.RGBA {
	b := src.Bounds()
	w, h := fit(b.Dx(), b.Dy(), side)
	cur, cw, ch := src, b.Dx(), b.Dy()
	for max(cw, ch) >= 2*side {
		cw, ch = cw/2, ch/2
		half := image.NewRGBA(image.Rect(0, 0, cw, ch))
		draw.ApproxBiLinear.Scale(half, half.Bounds(), cur, cur.Bounds(), draw.Src, nil)
		cur = half
	}
	rgba, ok := cur.(*image.RGBA)
	if !ok {
		rgba = image.NewRGBA(image.Rect(0, 0, cw, ch))
		draw.Draw(rgba, rgba.Bounds(), cur, cur.Bounds().Min, draw.Src)
	}
	if cw == w && ch == h {
		return rgba
	}
	return shrink(rgba, w, h)
}

// shrink averages src down to w×h: each pixel of the result is the mean of the
// part of src it covers, a pixel covered in part weighed by how much. Thin
// lines — the strokes of text in a screenshot — fade by their share instead
// of falling between samples.
func shrink(src *image.RGBA, w, h int) *image.RGBA {
	b := src.Bounds()
	cols, rows := spans(b.Dx(), w), spans(b.Dy(), h)
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y, row := range rows {
		for x, col := range cols {
			var sum [4]float64
			for _, ry := range row {
				line := src.PixOffset(b.Min.X, b.Min.Y+ry.at)
				for _, rx := range col {
					i, wt := line+4*rx.at, rx.share*ry.share
					for c := range 4 {
						sum[c] += wt * float64(src.Pix[i+c])
					}
				}
			}
			o := dst.PixOffset(x, y)
			for c := range 4 {
				dst.Pix[o+c] = uint8(min(sum[c]+0.5, 255))
			}
		}
	}
	return dst
}

// tap is one source position under a result position and its share of it.
type tap struct {
	at    int
	share float64
}

// spans maps each of n result positions to the source positions it covers out
// of size, with their shares; the shares of one result sum to 1.
func spans(size, n int) [][]tap {
	out := make([][]tap, n)
	step := float64(size) / float64(n)
	for i := range n {
		lo, hi := float64(i)*step, float64(i+1)*step
		for at := int(lo); at < size && float64(at) < hi; at++ {
			if covered := min(hi, float64(at+1)) - max(lo, float64(at)); covered > 0 {
				out[i] = append(out[i], tap{at: at, share: covered / step})
			}
		}
	}
	return out
}

// onWhite lays a picture over white. JPEG has no alpha: what was transparent
// would come out black.
func onWhite(img *image.RGBA) *image.RGBA {
	dst := image.NewRGBA(img.Bounds())
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), img, img.Bounds().Min, draw.Over)
	return dst
}

// encode writes img as PNG or as JPEG; nil when the result is over
// maxImageBytes.
func encode(img *image.RGBA, lossless bool) (*Image, error) {
	var buf bytes.Buffer
	mime := mimePNG
	var err error
	if lossless {
		err = png.Encode(&buf, img)
	} else {
		mime = mimeJPEG
		err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: jpegQuality})
	}
	if err != nil {
		return nil, fmt.Errorf("encode picture: %w", err)
	}
	if buf.Len() > maxImageBytes {
		return nil, nil
	}
	return &Image{MimeType: mime, Data: buf.Bytes(), Width: img.Bounds().Dx(), Height: img.Bounds().Dy()}, nil
}

// fit scales w×h down so the long side is at most side.
func fit(w, h, side int) (int, int) {
	long := max(w, h)
	if long <= side {
		return w, h
	}
	return max(w*side/long, 1), max(h*side/long, 1)
}
