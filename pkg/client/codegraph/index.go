package codegraph

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/sync/singleflight"
)

// Indexed reports whether dir carries a finished index. The marker, not the
// database: the engine writes index.db early and fills it as it goes, so a
// rebuild killed by a timeout leaves a file that looks complete and answers from
// half a graph. Ensure writes the marker after both passes, inside the index
// directory, so it goes when the worktree goes.
func (c *Client) Indexed(dir string) bool {
	_, err := os.Stat(readyPath(dir))
	return err == nil
}

// readyPath is the marker Indexed looks for.
func readyPath(dir string) string { return filepath.Join(dir, IndexDir, "ready") }

// Ensure indexes dir unless it is indexed already. Two questions about one
// commit must produce one indexing run.
func (c *Client) Ensure(ctx context.Context, dir string) error {
	if c.Indexed(dir) {
		return nil
	}
	// DoChan, so a caller who gives up stops waiting without abandoning an index
	// others still need. The work runs on a context that cannot be cancelled:
	// with the first caller's, its cancellation killed rebuild-index for
	// everybody who joined. index() and Finalize() still time out on their own.
	work := context.WithoutCancel(ctx)
	ch := c.group.DoChan(dir, func() (any, error) {
		err := c.index(work, dir)
		if err == nil {
			// The rebuild leaves the cross-file edges for the next start; doing
			// it here means the first question meets a finished index.
			err = c.Finalize(work, dir)
		}
		if err == nil {
			err = markReady(dir)
		}
		return nil, err
	})

	select {
	case res := <-ch:
		return res.Err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Finalize runs the engine's own incremental pass to completion.
//
// The engine finishes its index in the background on every start, and this
// client runs one process per question — so that background pass was killed
// halfway every time, the engine re-indexed all 610 files on the next start, and
// every query answered "Indexing in progress". Running the pass as its own
// synchronous command leaves the next query a complete index.
func (c *Client) Finalize(ctx context.Context, dir string) error {
	ctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.opts.Bin, "incremental-index") //nolint:gosec // fixed argv
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("codegraph: incremental-index in %s: %w: %s", dir, err, trimErr(out.String()))
	}
	return nil
}

// index builds the index from scratch, and times it: the first question about a
// commit pays seconds, and an unmeasured pause is an inexplicable one.
func (c *Client) index(ctx context.Context, dir string) error {
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, c.opts.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.opts.Bin, "rebuild-index", "--confirm") //nolint:gosec // fixed argv
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out

	if err := cmd.Run(); err != nil {
		observeIndex(time.Since(started), false)
		return fmt.Errorf("codegraph: rebuild-index in %s: %w: %s", dir, err, trimErr(out.String()))
	}
	observeIndex(time.Since(started), true)
	if c.onIndex != nil {
		c.onIndex()
	}
	return nil
}

// markReady records that both passes finished.
func markReady(dir string) error {
	if err := os.WriteFile(readyPath(dir), []byte("ok\n"), 0o600); err != nil {
		return fmt.Errorf("codegraph: mark index ready in %s: %w", dir, err)
	}
	return nil
}

// indexState is what Ensure needs; kept here so client.go stays about the
// protocol.
type indexState struct {
	group singleflight.Group
	// onIndex is a test hook: it counts how many indexing runs actually
	// happened, which is the only way to prove the single-flight works.
	onIndex func()
}
