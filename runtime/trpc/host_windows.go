//go:build windows

package trpcruntime

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"time"
)

// runHost starts a direct child with CommandContext.
// Windows process-tree cancellation has not been verified. Managed sandbox
// execution does not use this function.
func runHost(ctx context.Context, dir string, argv []string, env []string, timeout time.Duration) (string, string, int, bool, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = env
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
			if timedOut || ctx.Err() == nil {
				err = nil
			}
		}
	}
	return stdout.String(), stderr.String(), code, timedOut, err
}
