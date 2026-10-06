//go:build !windows

package examples_test

import (
	"os/exec"
	"syscall"
)

// stoppable readies an example to be stopped as the system stops a program.
func stoppable(*exec.Cmd) {}

// stop sends SIGTERM, as containers and service managers stop a program.
func stop(cmd *exec.Cmd) error { return cmd.Process.Signal(syscall.SIGTERM) }
