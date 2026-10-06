//go:build windows

package examples_test

import (
	"os/exec"
	"syscall"
)

var generateConsoleCtrlEvent = syscall.NewLazyDLL("kernel32.dll").NewProc("GenerateConsoleCtrlEvent")

// stoppable starts an example in a process group of its own, so Ctrl-Break reaches it alone.
func stoppable(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// stop sends Ctrl-Break, as Windows stops a console program: the example gets os.Interrupt.
func stop(cmd *exec.Cmd) error {
	if ok, _, err := generateConsoleCtrlEvent.Call(syscall.CTRL_BREAK_EVENT, uintptr(cmd.Process.Pid)); ok == 0 {
		return err
	}
	return nil
}
