package coding

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// GitStatus returns porcelain status. It does not stage or commit.
func GitStatus(ctx context.Context, root string) (string, error) {
	return git(ctx, root, "--no-optional-locks", "status", "--porcelain=v1", "--untracked-files=all")
}

// GitDiff returns the unstaged and staged patch for the workspace.
// It does not add, reset, or clean.
func GitDiff(ctx context.Context, root string) (string, error) {
	unstaged, err := git(ctx, root, "--no-optional-locks", "diff", "--no-ext-diff")
	if err != nil {
		return "", err
	}
	staged, err := git(ctx, root, "--no-optional-locks", "diff", "--cached", "--no-ext-diff")
	if err != nil {
		return "", err
	}
	return unstaged + staged, nil
}

func git(ctx context.Context, root string, args ...string) (string, error) {
	root, err := Resolve(root)
	if err != nil {
		return "", err
	}
	argv := append([]string{"-C", root}, args...)
	cmd := exec.CommandContext(ctx, "git", argv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}
