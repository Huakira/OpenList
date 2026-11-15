//go:build !linux

package icloud_photos

import "os/exec"

// detach is a no-op outside Linux: no Pdeathsig there.
func detach(cmd *exec.Cmd) {}
