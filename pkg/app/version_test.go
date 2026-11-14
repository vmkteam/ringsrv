package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/vmkteam/appkit"
)

// Not parallel: it sets the package-level value the linker would.
func TestVersion(t *testing.T) {
	assert.Equal(t, appkit.Version(), Version(), "without a link-time value the VCS stamp")

	version = "v0.0.6"
	t.Cleanup(func() { version = "" })
	assert.Equal(t, "v0.0.6", Version(), "the release the image was built as")
}
