//go:build windows

package pig_herdr

import (
	"context"
	"os/exec"
	"syscall"
)

// newCommand builds the Herdr command without a console window.
func newCommand(ctx context.Context, bin string, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd
}
