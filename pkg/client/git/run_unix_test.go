//go:build unix

package git

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// git clone and fetch are a tree of processes. Killing only the root left the
// helpers writing a pack nobody would use, and Run waited for them on the
// pipes they held, well past the deadline. The cancel takes the whole group.
func TestRun_CancelKillsTheWholeGroup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	// A stand-in for git: a helper that outlives the cancel, as remote-https
	// does, holding the same stdout.
	fake := filepath.Join(dir, "git")
	require.NoError(t, os.WriteFile(fake, []byte("#!/bin/sh\nsleep 60 &\necho $! > \"$PID_FILE\"\nwait\n"), 0o700))
	s := New(Options{Git: fake, Timeout: time.Minute})

	helper := func() int {
		b, err := os.ReadFile(pidFile)
		if err != nil {
			return 0
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		return pid
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cancelled := make(chan time.Time, 1)
	go func() {
		// Cancel once the helper is up: a freshly written script can take
		// seconds to start on a loaded machine, and a cancel that comes before
		// the helper exists proves nothing.
		for i := 0; i < 600 && helper() == 0; i++ {
			time.Sleep(50 * time.Millisecond)
		}
		cancelled <- time.Now()
		cancel()
	}()

	_, err := s.runEnv(ctx, dir, []string{"PID_FILE=" + pidFile}, "version")
	require.Error(t, err)
	assert.Less(t, time.Since(<-cancelled), 20*time.Second, "Run returns at the cancel, not when the helper is done")

	pid := helper()
	require.Positive(t, pid, "the helper was started")
	assert.Eventually(t, func() bool {
		return syscall.Kill(pid, 0) != nil
	}, 5*time.Second, 50*time.Millisecond, "the helper is killed with git")
}

// The first clone has a deadline of its own: the whole history comes as one
// pack, and the five minutes every other command gets are not enough for a
// large repository. A clone that hits it leaves nothing behind.
func TestEnsure_FirstCloneHasItsOwnTimeout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	fake := filepath.Join(root, "git")
	require.NoError(t, os.WriteFile(fake, []byte("#!/bin/sh\nsleep 60\n"), 0o700))
	s := New(Options{
		ReposDir:     filepath.Join(root, "repos"),
		WorktreesDir: filepath.Join(root, "worktrees"),
		Git:          fake,
		Timeout:      time.Minute,
		CloneTimeout: 300 * time.Millisecond,
	})

	start := time.Now()
	err := s.Ensure(t.Context(), Spec{Name: "apisrv", CloneURL: "https://git.example.com/apisrv.git"})
	require.ErrorContains(t, err, "git clone timed out after 300ms")
	assert.Less(t, time.Since(start), 20*time.Second)

	entries, err := os.ReadDir(s.opts.ReposDir)
	require.NoError(t, err)
	assert.Empty(t, entries, "the clone directory goes with the clone")
}
