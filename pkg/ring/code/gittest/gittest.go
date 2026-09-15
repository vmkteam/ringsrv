// Package gittest holds what the tests of this service need to build a real
// repository and to find the AST engine.
//
// It exists because three packages had byte-identical copies of the same git
// helper and two had the same engine lookup. Tests here run against real git
// and the real engine — a fake would test the fake — so the setup is worth
// sharing even though production code has no use for it.
package gittest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// EngineEnv overrides the engine binary, for a machine where it is not in PATH.
const EngineEnv = "RINGSRV_CODEGRAPH_BIN"

// engineBin is the binary the image ships and the tests look for.
const engineBin = "code-graph-mcp"

// Git runs git in dir with an author configured, so commits work on a machine
// with no global git config. It fails the test on error: a broken fixture is
// not a test result.
func Git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{
		"-C", dir,
		"-c", "user.email=t@t",
		"-c", "user.name=t",
		"-c", "commit.gpgsign=false",
	}, args...)

	out, err := exec.CommandContext(t.Context(), "git", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Write puts a file into dir, creating the directories it needs.
func Write(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

// EngineOrSkip returns the AST engine binary, skipping the test when the
// machine has none. The engine ships in the image, so its absence here
// is a property of the laptop rather than a failure.
func EngineOrSkip(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv(EngineEnv); bin != "" {
		return bin
	}
	bin, err := exec.LookPath(engineBin)
	if err != nil {
		t.Skipf("%s not installed; set %s to run this test", engineBin, EngineEnv)
	}
	return bin
}
