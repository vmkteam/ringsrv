package rpc

import (
	"fmt"

	"github.com/vmkteam/mcpkit/mcp"
)

// Pictures in one api_call answer. They are paid for apart from the byte
// budget of the JSON — a screenshot is a megabyte that a model reads as a
// picture, not as text — so they have a ceiling of their own.
const (
	maxImagesPerCall     = 4
	maxImageBytesPerCall = 3 << 20
)

// attachImages turns the pictures on the items into content blocks, in the
// order of the list, and points each item at its block; block 0 is the JSON
// answer itself. Past the call's ceiling a picture is dropped with its
// description, and an item the byte budget already dropped has none to carry.
// A repeat of the same call points at the block of the call it repeats instead
// of carrying the picture twice.
func attachImages(items []APICallItem) []mcp.ContentBlock {
	blocks := make([]mcp.ContentBlock, 0, min(len(items), maxImagesPerCall))
	size := 0
	for i := range items {
		img := items[i].img
		switch {
		case img == nil, items[i].Error != nil:
			continue
		case len(blocks) == maxImagesPerCall || size+len(img.Data) > maxImageBytesPerCall:
			truncateItem(&items[i], fmt.Sprintf("picture dropped: one answer carries at most %d pictures and %d bytes of them; ask for this file on its own",
				maxImagesPerCall, maxImageBytesPerCall))
			continue
		}
		size += len(img.Data)
		blocks = append(blocks, mcp.ImageBlock(img.Data, img.MimeType))
		items[i].Image = &ImageRef{
			Content: len(blocks), MimeType: img.MimeType,
			Width: img.Width, Height: img.Height, Bytes: len(img.Data),
		}
	}
	for i := range items {
		from := items[i].DedupOf
		switch {
		case from == nil || items[i].Error != nil:
		case items[*from].Error != nil:
			// The picture it repeats was dropped: so is the repeat, for the
			// same reason, or it would read as a file served with nothing in it.
			dup := *items[*from].Error
			items[i].Error, items[i].Truncated = &dup, true
		default:
			items[i].Image = items[*from].Image
		}
	}
	return blocks
}
