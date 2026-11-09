package plugin

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// InstallGit clones url at an optional revision and installs that tree.
// The recorded revision is the resolved commit. Later startups do not fetch.
func (m *Manager) InstallGit(ctx context.Context, url, revision string) (*Record, error) {
	url = strings.TrimSpace(url)
	if url == "" {
		return nil, fmt.Errorf("git url is empty")
	}
	dir, err := os.MkdirTemp("", "grape-plugin-git-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	args := []string{"clone"}
	if strings.TrimSpace(revision) == "" {
		args = append(args, "--depth", "1")
	}
	args = append(args, url, dir)
	if err := runGit(ctx, args...); err != nil {
		return nil, err
	}
	if strings.TrimSpace(revision) != "" {
		if err := runGit(ctx, "-C", dir, "checkout", revision); err != nil {
			return nil, err
		}
	}
	rev, err := gitOutput(ctx, "-C", dir, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	return m.installTree(dir, url, strings.TrimSpace(rev))
}

func runGit(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(stderr.String()))
	}
	return nil
}

func gitOutput(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
