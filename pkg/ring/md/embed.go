// Package md is the markdown that ships inside the binary: target cheat sheets
// served as MCP resources and investigation scenarios served as MCP prompts.
//
// Only the tree and the test that checks it against the catalogue live here.
// What indexes it is mcpkit/doc, which takes this FS and this Root — the loader
// used to sit beside the tree, and it moved to the library because three
// services were loading the same markdown the same way.
package md

import "embed"

// FS holds the tree. It lives beside the test that checks it against the
// catalogue: a sheet for a target nobody can reach is the failure this
// arrangement is meant to catch.
//
//go:embed md
var FS embed.FS

// Root is the path FS is rooted at — resource URIs are built from paths
// relative to it (md/targets/prom.md → ringsrv://targets/prom.md).
const Root = "md"

// URIScheme prefixes every resource URI of this service. It is here rather
// than in the loader because the scheme is the service's name for its own
// catalogue, and the loader is shared.
const URIScheme = "ringsrv://"
