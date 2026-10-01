package files

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"testing"

	"github.com/vmkteam/ringsrv/pkg/ring/files/filestest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

func solid(w, h int, c color.Color) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, c)
		}
	}
	return img
}

// noise is a picture no encoder can compress: left half transparent, right
// half random.
func noise(w, h int) *image.NRGBA {
	r := rand.New(rand.NewPCG(1, 2))
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := w / 2; x < w; x++ {
			img.Set(x, y, color.NRGBA{R: uint8(r.UintN(256)), G: uint8(r.UintN(256)), B: uint8(r.UintN(256)), A: 255})
		}
	}
	return img
}

func newTestFetcher() *Fetcher { return NewFetcher(nil) }

func TestImage_SmallPassesThrough(t *testing.T) {
	t.Parallel()
	data := filestest.PNG(t, 40, 30, color.RGBA{R: 200, A: 255})
	img, err := newTestFetcher().picture(context.Background(), data)
	require.NoError(t, err)
	assert.Equal(t, "image/png", img.MimeType)
	assert.Equal(t, data, img.Data, "a picture that fits travels as it came")
	assert.Equal(t, 40, img.Width)
	assert.Equal(t, 30, img.Height)
}

// The format comes from the bytes, whatever the upstream called the file.
func TestImage_FormatFromBytes(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, solid(20, 20, color.White), nil))
	img, err := newTestFetcher().picture(context.Background(), buf.Bytes())
	require.NoError(t, err)
	assert.Equal(t, "image/jpeg", img.MimeType)

	_, err = newTestFetcher().picture(context.Background(), []byte("<svg xmlns='http://www.w3.org/2000/svg'/>"))
	assert.Equal(t, CodeUnsupported, refusalCode(err))
}

func TestImage_GIFAndWebP(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	pal := image.NewPaletted(image.Rect(0, 0, 8, 8), []color.Color{color.Black, color.White})
	require.NoError(t, gif.Encode(&buf, pal, nil))
	img, err := newTestFetcher().picture(context.Background(), buf.Bytes())
	require.NoError(t, err)
	assert.Equal(t, "image/gif", img.MimeType)

	// A 1×1 lossless WebP.
	webp, err := base64.StdEncoding.DecodeString("UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA==")
	require.NoError(t, err)
	img, err = newTestFetcher().picture(context.Background(), webp)
	require.NoError(t, err)
	assert.Equal(t, "image/webp", img.MimeType)
	assert.Equal(t, 1, img.Width)
}

// A wide screenshot is scaled to the long side and stays PNG.
func TestImage_ScaledDown(t *testing.T) {
	t.Parallel()
	data := filestest.PNG(t, 3000, 1000, color.RGBA{B: 255, A: 255})
	img, err := newTestFetcher().picture(context.Background(), data)
	require.NoError(t, err)
	assert.Equal(t, "image/png", img.MimeType)
	assert.Equal(t, maxImageSide, img.Width)
	assert.Equal(t, 522, img.Height)
	cfg, err := png.DecodeConfig(bytes.NewReader(img.Data))
	require.NoError(t, err)
	assert.Equal(t, maxImageSide, cfg.Width)
}

// A PNG that does not fit as PNG goes out as JPEG, and what was transparent is
// white rather than black.
func TestImage_HeavyPNGBecomesJPEG(t *testing.T) {
	t.Parallel()
	data := encodePNG(t, noise(1800, 1200))
	img, err := newTestFetcher().picture(context.Background(), data)
	require.NoError(t, err)
	assert.Equal(t, "image/jpeg", img.MimeType)
	assert.LessOrEqual(t, len(img.Data), maxImageBytes)

	out, err := jpeg.Decode(bytes.NewReader(img.Data))
	require.NoError(t, err)
	r, g, b, _ := out.At(10, 10).RGBA()
	assert.Greater(t, r>>8, uint32(240), "transparent becomes white")
	assert.Greater(t, g>>8, uint32(240))
	assert.Greater(t, b>>8, uint32(240))
}

// A header that declares a huge canvas is refused before a pixel is decoded.
func TestImage_HugeCanvasRefused(t *testing.T) {
	t.Parallel()
	data := filestest.PNG(t, 1, 1, color.White)
	// IHDR starts at byte 8: length, "IHDR", width, height, then the rest and
	// the CRC over type and data.
	binary.BigEndian.PutUint32(data[16:], 30000)
	binary.BigEndian.PutUint32(data[20:], 30000)
	binary.BigEndian.PutUint32(data[29:], crc32.ChecksumIEEE(data[12:29]))

	_, err := newTestFetcher().picture(context.Background(), data)
	assert.Equal(t, CodeTooLarge, refusalCode(err))
}

// A picture that fits is still decoded whole before it travels as it came: a
// broken one would be refused by whoever reads the block.
func TestImage_BrokenRefused(t *testing.T) {
	t.Parallel()
	data := filestest.PNG(t, 40, 30, color.RGBA{G: 200, A: 255})
	idat := bytes.Index(data, []byte("IDAT"))
	require.Positive(t, idat)
	for i := idat + 4; i < idat+12; i++ {
		data[i] ^= 0xFF // the compressed pixels, not the header
	}
	_, err := newTestFetcher().picture(context.Background(), data)
	assert.Equal(t, CodeUnsupported, refusalCode(err))
}

// What a decode takes is read off the header: 16 bits a channel is twice the
// pixels of 8, and a progressive JPEG keeps its coefficients as well.
func TestImage_DecodeCost(t *testing.T) {
	t.Parallel()
	deep := image.NewRGBA64(image.Rect(0, 0, 1, 1))
	data := encodePNG(t, deep)
	binary.BigEndian.PutUint32(data[16:], 5000)
	binary.BigEndian.PutUint32(data[20:], 4000)
	binary.BigEndian.PutUint32(data[29:], crc32.ChecksumIEEE(data[12:29]))
	_, err := newTestFetcher().picture(context.Background(), data)
	require.Equal(t, CodeTooLarge, refusalCode(err), "20 Mpx at 8 bytes a pixel is refused before decoding")
	assert.Contains(t, err.Error(), "to decode")

	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, solid(64, 48, color.White), nil))
	frame, ok := readJPEGFrame(buf.Bytes())
	require.True(t, ok)
	assert.Equal(t, jpegFrame{components: 3, samples: 1.5}, frame, "Go writes 4:2:0: a plane at full size and two at a quarter")
	assert.Equal(t, int64(1.5*64*48), frame.cost(64*48))

	progressive := bytes.Replace(buf.Bytes(), []byte{0xFF, 0xC0}, []byte{0xFF, 0xC2}, 1)
	frame, ok = readJPEGFrame(progressive)
	require.True(t, ok)
	assert.True(t, frame.progressive)
	assert.Equal(t, int64((1.5+4*1.5)*64*48), frame.cost(64*48), "the coefficients of every sample come on top")
}

// Each pixel of the result is the mean of what it covers, a part covered in part
// weighed by its share.
func TestShrink(t *testing.T) {
	t.Parallel()
	src := image.NewRGBA(image.Rect(0, 0, 3, 1))
	for x, v := range []uint8{0, 90, 180} {
		src.SetRGBA(x, 0, color.RGBA{R: v, G: v, B: v, A: 255})
	}
	dst := shrink(src, 2, 1)
	assert.Equal(t, color.RGBA{R: 30, G: 30, B: 30, A: 255}, dst.RGBAAt(0, 0), "all of 0 and half of 90")
	assert.Equal(t, color.RGBA{R: 150, G: 150, B: 150, A: 255}, dst.RGBAAt(1, 0), "half of 90 and all of 180")
}

// A picture past twice the side is halved first, then averaged to the size.
func TestScale_Halves(t *testing.T) {
	t.Parallel()
	blue := color.RGBA{B: 255, A: 255}
	src := image.NewRGBA(image.Rect(0, 0, 5000, 3000))
	for i := 0; i < len(src.Pix); i += 4 {
		copy(src.Pix[i:], []uint8{blue.R, blue.G, blue.B, blue.A})
	}
	got := scale(src, maxImageSide)
	assert.Equal(t, image.Rect(0, 0, maxImageSide, 940), got.Bounds())
	assert.Equal(t, blue, got.RGBAAt(0, 0))
	assert.Equal(t, blue, got.RGBAAt(maxImageSide-1, 939))
}
