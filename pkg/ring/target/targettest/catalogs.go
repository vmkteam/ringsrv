// Package targettest resolves the catalogues that ship with the repository,
// so tests check the bytes that get deployed instead of a copy of them.
//
// A contour's catalogue is not a file of its own: the body lives in the
// `targets` heredoc of deployments/<contour>.vars.hcl, which Nomad renders into
// secrets/targets.toml together with the tokens. cfg/targets.toml.dist stays
// behind as the readable example — the drift test in pkg/app keeps it equal to
// the prod body byte for byte.
//
// This package does not import target: those catalogue tests are in-package, and
// importing back would be a cycle. It hands out paths and bytes.
package targettest

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// ConfigChecksEnv gates the checks whose subject is the shipped configuration
// rather than the code: deployment templates, sample configs, catalogues in git.
// They fail on an edit to a config, which is work for whoever makes that edit —
// but they live in ./pkg/app and ./pkg/ring/target, which CI runs whole, so
// leaving the target out of CI was not enough to keep them off every branch.
// `make config-checks` sets this and runs them next to the edit.
const ConfigChecksEnv = "RINGSRV_CHECK_CONFIGS"

// SkipUnlessConfigChecks skips a shipped-config check unless it was asked for.
// Call it first in the test body, the way the live-database tests skip on a
// missing DSN.
func SkipUnlessConfigChecks(t *testing.T) {
	t.Helper()
	if os.Getenv(ConfigChecksEnv) == "" {
		t.Skipf("%s is not set: shipped-config checks run from `make config-checks`", ConfigChecksEnv)
	}
}

// The contours a catalogue is shipped for, in the order tests report them.
const (
	Prod  = "prod"
	Dev   = "dev"
	Local = "local"
)

// Example is the readable copy of the prod catalogue, kept in cfg/ because a
// 40 KB heredoc inside a deployment file is not how anyone learns the format.
var Example = filepath.Join("cfg", "targets.toml.dist")

// source names the file each contour's body is carried by. Only the laptop copy
// is still a catalogue file of its own: it carries literal stubs instead of
// Nomad placeholders, and `make init` copies it.
var source = map[string]string{
	Prod:  filepath.Join("deployments", "master.vars.hcl"),
	Dev:   filepath.Join("deployments", "devel.vars.hcl"),
	Local: filepath.Join("cfg", "targets.local.toml.dist"),
}

// targetsHeredoc picks the catalogue out of a vars file. Anchored at the line
// start and terminated by a line of its own: the same file carries the config
// heredoc above it, and the catalogue itself contains neither `${` nor a line
// that reads EOF.
var targetsHeredoc = regexp.MustCompile(`(?sm)^targets\s*=\s*<<-EOF$\n(.*?)\n^EOF$`)

// Catalog is one shipped catalogue.
type Catalog struct {
	Name   string // contour: Prod, Dev or Local
	Source string // repository-relative file the body came from, for messages
	Path   string // a file target.Load can open; temporary when the body was extracted
}

// shipped names a contour's source file and locates it, or skips the test when
// that file is not in this checkout. The public mirror carries the code and the
// example catalogue but none of the deployment files, so there a check that
// reads a deployed body has nothing to read. Skipping keeps that tree green
// without putting these tests behind ConfigChecksEnv, which would also take
// them out of the CI that does have the files.
func shipped(t *testing.T, contour string) (file, path string) {
	t.Helper()

	file = source[contour]
	require.NotEmpty(t, file, "unknown contour %q", contour)

	path = filepath.Join(Root(t), file)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		t.Skipf("%s is not in this checkout: the shipped catalogues live in the internal repository", file)
	}
	return file, path
}

// Body returns the catalogue body of a contour, ready to parse as TOML, with the
// consul-template control lines dropped. The {{ .token }} placeholders stay:
// they are TOML strings, and the catalogue validates with them exactly as it
// does after the Nomad render.
func Body(t *testing.T, contour string) []byte {
	t.Helper()

	file, path := shipped(t, contour)

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	if filepath.Ext(file) != ".hcl" {
		return raw
	}

	m := targetsHeredoc.FindSubmatch(raw)
	require.Len(t, m, 2, "%s: no `targets = <<-EOF ... EOF` block", file)

	var out strings.Builder
	for line := range strings.SplitSeq(string(m[1]), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "{{-") {
			continue
		}
		out.WriteString(line + "\n")
	}
	return []byte(out.String())
}

// Catalogs returns every shipped catalogue. Call it from the parent test: the
// temporary files it makes outlive parallel subtests, which finish before the
// parent's cleanup runs.
func Catalogs(t *testing.T) []Catalog {
	t.Helper()

	out := make([]Catalog, 0, len(source))
	for _, name := range []string{Prod, Dev, Local} {
		out = append(out, Catalog{Name: name, Source: source[name], Path: Path(t, name)})
	}
	return out
}

// Path returns a file holding the contour's catalogue, for the tests that go
// through target.Load. A body carried by a vars file is written to a temporary
// file; a catalogue that is a file of its own is named where it lies, so a
// failure points at a path the reader can open.
func Path(t *testing.T, contour string) string {
	t.Helper()

	file, path := shipped(t, contour)
	if filepath.Ext(file) != ".hcl" {
		return path
	}

	p := filepath.Join(t.TempDir(), "targets.toml")
	require.NoError(t, os.WriteFile(p, Body(t, contour), 0o600))
	return p
}

// Root returns the repository root. Tests run in their own package directory
// and the deployment files are addressed from the top, so counting ".." per
// package is one rename away from being wrong.
func Root(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, parent, dir, "no go.mod above %s", dir)
		dir = parent
	}
}
