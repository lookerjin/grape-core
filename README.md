# grape-core

Composable Go agent SDK on [tRPC-Agent-Go](https://github.com/trpc-group/trpc-agent-go) v1.12.0.

```go
import (
    "github.com/lookerjin/grape-core/agent"
    "github.com/lookerjin/grape-core/config"
    trpcruntime "github.com/lookerjin/grape-core/runtime/trpc"
)
```

The module path is `github.com/lookerjin/grape-core`. The `go` line is `1.27.0`. `runtime/trpc` uses SQLite through `github.com/mattn/go-sqlite3`, so builds need cgo and a C compiler. This module is not cgo-free.

```bash
go build ./...
go vet ./...
go test ./...
```

```bash
go run ./examples/basic
go run ./examples/coding
go run ./examples/extensions ./path/to/plugin
```

Examples use a mock model unless `GRAPE_REAL_MODEL=1`. A real call also needs `MODEL_API_KEY` and, for a non-default endpoint, `MODEL_BASE_URL`. Keys stay in the environment named by `model.api_key_env`.

## Data directory

- Unix: `~/.grape-core`
- Windows: `%AppData%\grape-core`

Sessions, plugin installs, memory notes, and the optional journal live under that directory unless config paths override them.

## Safety

`workspace_write` with `sandbox.mode: managed` runs commands through the tRPC OS sandbox after a preflight. Failure returns `agent.ErrSandboxUnavailable` and does not fall back to a host shell. `danger_full_access` is explicit and reports isolation `none`.

Linux bubblewrap was exercised here. Writes outside the workspace grant are denied, and restricted networking cannot reach a host loopback listener. Existing sensitive directories (such as `~/.ssh` and the grape-core data directory) are masked with `WithNoAccessPaths` and listed in `Capabilities.SensitivePathsMasked`. Ordinary host files stay readable, so `FilesystemIsolation` is false. macOS Seatbelt was not executed here. Windows managed sandbox is unsupported. See [docs/sandbox.md](docs/sandbox.md).

Memory and evolution are off by default. Goal mode is per request (`agent.ModeGoal`) and uses the tRPC goal extension with `goal.max_iterations` as the retry budget. Shell hooks run through the sandbox executor. Stdio MCP does not: it starts on the host with isolation `none`, a minimal environment, and only when that server sets `trust: host`. A URL without `transport` is treated as HTTP (`sse` or `streamable`), not stdio.

## Docs

- [docs/architecture.md](docs/architecture.md)
- [docs/api.md](docs/api.md)
- [docs/plugins.md](docs/plugins.md)
- [docs/sandbox.md](docs/sandbox.md)
- [docs/design-spec.md](docs/design-spec.md)
