# Architecture

grape-core is one Go module, `github.com/lookerjin/grape-core`, pinned to Go 1.27.0 and tRPC-Agent-Go v1.12.0 (`trpc-agent-go` and `trpc-agent-go/session/sqlite`).

Public packages do not import `trpc.group/...`:

- `agent` is the run, event, approval, and error protocol.
- `config` loads YAML. Later files override earlier ones. `${NAME}` and `${NAME:-default}` expand before decoding.
- `plugin` installs bundles and returns a component list. It does not load skills, MCP, or hooks itself.
- `mcp`, `skill`, `hooks`, `rules`, and `memory` manage their own files and records.
- `coding` checks workspace paths and reads git status and diff. It does not add, commit, push, reset, or clean.
- `sandbox` is the isolation contract: capabilities, an executor, and a backend extension point.

`runtime/trpc` is the only interface that constructs tRPC models, runners, sessions, skill repositories, and the OS sandbox. A caller can use `skill` or `plugin` without creating a runner.

`Runtime.Start` uses the caller context. `TaskHost.Start` owns a separate context so a UI can exit without defining that behavior for every `Start` call.

Memory and evolution are off unless `memory.enabled` or `evolution.enabled` is set. Goal mode is selected per run with `agent.ModeGoal` and installs the tRPC goal extension for that run only. Graph runs use `UpperGraph` or `Options.FixedAgent` and still go through the same runner and session service.
