//go:build unix

package proc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A command whose child outlives the cancel: without the group kill, Run waits
// for the child on the stdout it holds, here for a minute.
func TestKillGroupOnCancel(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	child := func() int {
		b, err := os.ReadFile(pidFile)
		if err != nil {
			return 0
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		return pid
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", `sleep 60 & echo $! > "$0"; wait`, pidFile)
	var out strings.Builder
	cmd.Stdout = &out
	KillGroupOnCancel(cmd)
	require.NoError(t, cmd.Start())

	require.Eventually(t, func() bool { return child() != 0 }, 30*time.Second, 20*time.Millisecond, "the child starts")
	cancelled := time.Now()
	cancel()

	require.Error(t, cmd.Wait())
	assert.Less(t, time.Since(cancelled), 20*time.Second, "Wait returns at the cancel, not when the child is done")
	assert.Eventually(t, func() bool {
		return syscall.Kill(child(), 0) != nil
	}, 5*time.Second, 50*time.Millisecond, "the child is killed with its parent")
}
