//go:build unix

// Package proc holds what the clients of external programs — git, the AST
// engine — share about running them.
package proc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// KillGroupOnCancel starts the command in a process group of its own and makes
// the cancel kill the whole group.
//
// The default cancel kills the direct child only. git clone and fetch are a
// tree — remote-https runs index-pack — and the AST engine installed from npm
// is a node shim around the real binary. The rest of the tree went on writing
// and held the pipes open, so Run returned minutes past its deadline. A
// process that leaves the group on purpose (git's detached gc calls setsid) is
// not touched.
func KillGroupOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			// The group finished between the deadline and the kill: Run reports
			// the command's own exit status then, as with the default cancel.
			return os.ErrProcessDone
		}
		return err
	}
}
