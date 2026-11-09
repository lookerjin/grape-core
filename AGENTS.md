# grape-core engineering constraints

This repository is a single Go module, `github.com/lookerjin/grape-core`. It is an in-process SDK. It does not ship a TUI, desktop app, or network service.

- Go version is `1.27.0` (`go` and `toolchain` in `go.mod`).
- `trpc-agent-go` and `trpc-agent-go/session/sqlite` stay pinned to `v1.12.0`. Do not depend on `@main` or `@latest`.
- Do not copy upstream `replace` directives into this module.
- Public domain packages (`agent`, `config`, `plugin`, `mcp`, `skill`, `hooks`, `rules`, `memory`, `coding`, `sandbox`) must not import `trpc.group/...`. Adaptation lives in `runtime/trpc`.
- Reuse tRPC Agent, Runner, Session, Model, Tool, MCP, Skill, Plugin, and CodeExecutor. Do not reimplement those subsystems.
- `llmagent.WithEnableCodeExecutionResponseProcessor(false)` is mandatory. Skills must not fall back to an implicit local code executor.
- `workspace_write` uses the managed OS sandbox. If the preflight fails, command execution returns `agent.ErrSandboxUnavailable`. It must not fall back to a host shell.
- Windows has no managed native sandbox in this tree. That path returns `agent.ErrSandboxUnavailable`. Do not describe Job Objects, WSL, or Docker as that backend.
- API keys are read from the environment or an injected model. They are not written into config files, events, or journals.
- Default data directory: `~/.grape-core` on Unix, `%AppData%\grape-core` on Windows.
- Memory and Evolution stay off unless the config enables them.
