//go:build !linux && !darwin

package renew

import "os/exec"

func prepareCommand(*exec.Cmd) {}

func killProcessTree(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
