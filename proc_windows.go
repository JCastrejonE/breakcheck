//go:build windows

package breakcheck

import (
	"context"
	"os/exec"
	"time"
)

// On Windows a timeout kills the shell but not its children; there is no
// process group here. Keep suites that spawn servers on a short leash.
func shellCommand(ctx context.Context, script, dir string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "cmd", "/C", script)
	cmd.Dir = dir
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

func killGroup(*exec.Cmd) {}
