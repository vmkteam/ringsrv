//go:build !unix

// Package proc holds what the clients of external programs — git, the AST
// engine — share about running them.
package proc

import "os/exec"

// KillGroupOnCancel has no process groups to use here: the default cancel
// kills the direct child and leaves the rest to the pipes they lose.
func KillGroupOnCancel(*exec.Cmd) {}
