//go:build !windows

package pig_herdr

import (
	"context"
	"os/exec"
)

func newCommand(ctx context.Context, bin string, args []string) *exec.Cmd {
	return exec.CommandContext(ctx, bin, args...)
}
