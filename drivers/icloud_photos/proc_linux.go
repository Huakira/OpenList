//go:build linux

package icloud_photos

import (
	"os/exec"
	"syscall"
)

// detach makes the bridge die with OpenList instead of outliving it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
