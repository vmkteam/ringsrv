package app

import "github.com/vmkteam/appkit"

// version is the release the binary was built as, set at link time:
//
//	go build -ldflags "-X github.com/vmkteam/ringsrv/pkg/app.version=v0.0.6"
//
// The image is built without .git (see .dockerignore), so the VCS stamp
// appkit.Version reads is not there, and every image reported "devel" — to MCP
// clients in serverInfo, to upstreams in the User-Agent, and to Sentry as the
// release, where no two releases could be told apart.
var version string

// Version is the release set at link time, or the VCS revision appkit reads
// from the build info when there is none — a local `go build` has the
// repository at hand.
func Version() string {
	if version != "" {
		return version
	}
	return appkit.Version()
}
