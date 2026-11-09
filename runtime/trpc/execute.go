package trpcruntime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/coding"
	"github.com/lookerjin/grape-core/config"
	"github.com/lookerjin/grape-core/sandbox"
	"trpc.group/trpc-go/trpc-agent-go/codeexecutor"
	trpcsandbox "trpc.group/trpc-go/trpc-agent-go/codeexecutor/sandbox"
)

// commandExecutor enforces workspace permission and sandbox mode.
//
// platform is runtime.GOOS in production. Tests set it to "windows" to exercise
// the fail-closed path without a Windows machine. That simulation is not a
// Windows verification.
type commandExecutor struct {
	cfg         config.Config
	platform    string
	sandboxRoot string
	dataDir     string
	external    sandbox.Backend

	mu     sync.Mutex
	probed bool
	caps   sandbox.Capabilities
}

func newCommandExecutor(cfg config.Config, sandboxRoot, dataDir, platform string, external sandbox.Backend) *commandExecutor {
	if platform == "" {
		platform = runtime.GOOS
	}
	return &commandExecutor{
		cfg:         cfg,
		platform:    platform,
		sandboxRoot: sandboxRoot,
		dataDir:     dataDir,
		external:    external,
	}
}

func (e *commandExecutor) Capabilities(ctx context.Context) (sandbox.Capabilities, error) {
	if e.cfg.Workspace.Permission == agent.PermissionReadOnly || e.cfg.Sandbox.Mode != sandbox.ModeManaged {
		return sandbox.Capabilities{
			Platform: e.platform,
			Reason:   "managed sandbox is not the active policy",
		}, nil
	}
	return e.probe(ctx)
}

func (e *commandExecutor) probe(ctx context.Context) (sandbox.Capabilities, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.probed {
		return e.caps, nil
	}
	e.probed = true
	if e.platform == "windows" {
		e.caps = sandbox.Assess(sandbox.Probe{Platform: "windows"})
		return e.caps, nil
	}
	var preflight error
	rt := trpcsandbox.NewRuntime(
		trpcsandbox.WithWorkspaceRoot(e.sandboxRoot),
		trpcsandbox.WithPermissionProfile(e.profile("")),
	)
	defer rt.Close()
	ws, err := rt.CreateWorkspace(ctx, "probe", codeexecutor.WorkspacePolicy{})
	if err != nil {
		preflight = err
	} else if _, err = rt.RunProgram(ctx, ws, codeexecutor.RunProgramSpec{
		Cmd:     "/bin/true",
		Timeout: 20 * time.Second,
	}); err != nil {
		preflight = err
	}
	masked, err := e.denyList("")
	if err != nil && preflight == nil {
		preflight = err
	}
	e.caps = sandbox.Assess(sandbox.Probe{
		Platform:           e.platform,
		ManagedImplemented: true,
		PreflightErr:       preflight,
		NetworkRestricted:  e.cfg.Sandbox.Network != "enabled",
		SensitivePaths:     masked.Paths,
	})
	return e.caps, nil
}

func (e *commandExecutor) Execute(ctx context.Context, req sandbox.Request) (sandbox.Result, error) {
	if len(req.Argv) == 0 || strings.TrimSpace(req.Argv[0]) == "" {
		return sandbox.Result{}, agent.Invalid("command", fmt.Errorf("argv is empty"))
	}
	root, err := coding.Resolve(req.Workspace)
	if err != nil {
		return sandbox.Result{}, agent.Wrap(agent.ErrPermissionDenied, "workspace", err)
	}
	req.Workspace = root
	switch e.cfg.Workspace.Permission {
	case agent.PermissionReadOnly:
		return sandbox.Result{}, agent.Wrap(agent.ErrPermissionDenied, "command", fmt.Errorf("read_only workspace"))
	case agent.PermissionDangerFullAccess:
		return e.host(ctx, req)
	case agent.PermissionWorkspaceWrite:
		return e.executeWrite(ctx, req)
	default:
		return sandbox.Result{}, agent.Invalid("permission", fmt.Errorf("unsupported permission %q", e.cfg.Workspace.Permission))
	}
}

func (e *commandExecutor) executeWrite(ctx context.Context, req sandbox.Request) (sandbox.Result, error) {
	if _, err := e.denyList(req.Workspace); err != nil {
		return sandbox.Result{}, agent.Invalid("sandbox.deny_read", err)
	}
	switch e.cfg.Sandbox.Mode {
	case sandbox.ModeDangerFullAccess:
		return e.host(ctx, req)
	case sandbox.ModeExternal:
		return e.externalRun(ctx, req)
	default:
		caps, err := e.probe(ctx)
		if err != nil {
			return sandbox.Result{}, err
		}
		if !caps.ManagedIsolation {
			reason := caps.Reason
			if reason == "" {
				reason = "managed sandbox is unavailable"
			}
			return sandbox.Result{}, agent.Wrap(agent.ErrSandboxUnavailable, "command", fmt.Errorf("%s", reason))
		}
		return e.managed(ctx, req)
	}
}

func (e *commandExecutor) externalRun(ctx context.Context, req sandbox.Request) (sandbox.Result, error) {
	if e.external == nil {
		return sandbox.Result{}, agent.Wrap(agent.ErrSandboxUnavailable, "external", fmt.Errorf("no backend"))
	}
	if err := e.external.Healthy(ctx); err != nil {
		return sandbox.Result{}, agent.Wrap(agent.ErrSandboxUnavailable, "external", err)
	}
	res, err := e.external.Execute(ctx, req)
	if err != nil {
		return sandbox.Result{}, err
	}
	if res.Isolation == "" {
		res.Isolation = sandbox.IsolationExternal
	}
	return res, nil
}

func (e *commandExecutor) host(ctx context.Context, req sandbox.Request) (sandbox.Result, error) {
	stdout, stderr, code, timedOut, err := runHost(ctx, req.Workspace, req.Argv, envList(req.Env), req.Timeout)
	if err != nil {
		if ctx.Err() != nil && !timedOut {
			return sandbox.Result{Stdout: stdout, Stderr: stderr, ExitCode: code, Isolation: sandbox.IsolationNone}, agent.Wrap(agent.ErrRunCancelled, "command", err)
		}
		return sandbox.Result{Stdout: stdout, Stderr: stderr, ExitCode: code, Isolation: sandbox.IsolationNone, TimedOut: timedOut}, agent.Wrap(agent.ErrTool, "command", err)
	}
	return sandbox.Result{Stdout: stdout, Stderr: stderr, ExitCode: code, Isolation: sandbox.IsolationNone, TimedOut: timedOut}, nil
}

func (e *commandExecutor) managed(ctx context.Context, req sandbox.Request) (sandbox.Result, error) {
	if err := os.MkdirAll(e.sandboxRoot, 0o755); err != nil {
		return sandbox.Result{}, agent.Wrap(agent.ErrSandboxUnavailable, "sandbox root", err)
	}
	rt := trpcsandbox.NewRuntime(
		trpcsandbox.WithWorkspaceRoot(e.sandboxRoot),
		trpcsandbox.WithPermissionProfile(e.profile(req.Workspace)),
	)
	defer rt.Close()
	ws, err := rt.CreateWorkspace(ctx, "run", codeexecutor.WorkspacePolicy{})
	if err != nil {
		return sandbox.Result{}, agent.Wrap(agent.ErrSandboxUnavailable, "workspace", err)
	}
	cache := filepath.Join(req.Workspace, ".grape-cache")
	tmp := filepath.Join(req.Workspace, ".grape-tmp")
	for _, dir := range []string{cache, tmp} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return sandbox.Result{}, err
		}
	}
	env := map[string]string{
		"GOCACHE":             cache,
		"GOTMPDIR":            tmp,
		"HOME":                req.Workspace,
		"GOTELEMETRY":         "off",
		"GOTOOLCHAIN":         "local",
		"GIT_CONFIG_NOSYSTEM": "1",
	}
	for k, v := range req.Env {
		env[k] = v
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	spec := codeexecutor.RunProgramSpec{
		Cmd:     "sh",
		Args:    append([]string{"-c", `cd "$1" && shift && exec "$@"`, "sh", req.Workspace}, req.Argv...),
		Env:     env,
		Timeout: timeout,
		Cwd:     "work",
	}
	res, err := rt.RunProgram(ctx, ws, spec)
	out := sandbox.Result{
		Stdout:    res.Stdout,
		Stderr:    res.Stderr,
		ExitCode:  res.ExitCode,
		Isolation: sandbox.IsolationManaged,
		TimedOut:  res.TimedOut,
	}
	if err != nil {
		if strings.Contains(err.Error(), "UnsupportedBackend") || strings.Contains(err.Error(), "not implemented") {
			return sandbox.Result{}, agent.Wrap(agent.ErrSandboxUnavailable, "command", err)
		}
		if res.TimedOut {
			return out, agent.Wrap(agent.ErrTool, "command", err)
		}
		if ctx.Err() != nil {
			return out, agent.Wrap(agent.ErrRunCancelled, "command", ctx.Err())
		}
		return out, agent.Wrap(agent.ErrTool, "command", err)
	}
	return out, nil
}

func (e *commandExecutor) profile(workspace string) trpcsandbox.PermissionProfile {
	profile := trpcsandbox.WorkspaceWriteProfile()
	if e.cfg.Workspace.Permission == agent.PermissionReadOnly {
		profile = trpcsandbox.ReadOnlyProfile()
	}
	if e.cfg.Sandbox.Network == "enabled" {
		profile = profile.WithNetworkPolicy(trpcsandbox.NetworkPolicy{Mode: trpcsandbox.NetworkEnabled})
	}
	if workspace != "" && e.cfg.Workspace.Permission != agent.PermissionReadOnly {
		profile = profile.WithWritePaths(workspace)
	}
	denied, err := e.denyList(workspace)
	if err != nil {
		return profile
	}
	if len(denied.Paths) > 0 {
		profile = profile.WithNoAccessPaths(denied.Paths...)
	}
	if len(denied.Globs) > 0 {
		profile = profile.WithNoAccessGlobs(denied.Globs...)
	}
	return profile
}

func (e *commandExecutor) denyList(workspace string) (sandbox.DenyList, error) {
	home, _ := os.UserHomeDir()
	return sandbox.SensitivePaths(sandbox.DenyRequest{
		Home:            home,
		DataDir:         e.dataDir,
		SessionPath:     e.cfg.Session.Path,
		Extra:           e.cfg.Sandbox.DenyRead,
		Mode:            e.cfg.Sandbox.DenyReadMode,
		CredentialFiles: credentialFiles(e.cfg),
		Workspace:       workspace,
	})
}

func credentialFiles(cfg config.Config) []string {
	var out []string
	out = append(out, fileRef(cfg.MCP.Servers)...)
	return out
}

func fileRef(servers []config.MCPServerConfig) []string {
	var out []string
	for _, server := range servers {
		if strings.HasPrefix(server.CredentialRef, "file:") {
			out = append(out, server.CredentialRef)
		}
		for _, value := range server.Headers {
			if strings.HasPrefix(value, "file:") {
				out = append(out, value)
			}
		}
	}
	return out
}

func envList(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	base := os.Environ()
	for k, v := range env {
		base = append(base, k+"="+v)
	}
	return base
}
