# grape-core

Composable Go agent SDK on [tRPC-Agent-Go](https://github.com/trpc-group/trpc-agent-go) `v1.12.0`.

```go
import (
    "github.com/lookerjin/grape-core/agent"
    "github.com/lookerjin/grape-core/config"
    trpcruntime "github.com/lookerjin/grape-core/runtime/trpc"
)
```

The module path is `github.com/lookerjin/grape-core`. Go `1.27.0` is required. `runtime/trpc` uses SQLite through `github.com/mattn/go-sqlite3`, so builds need cgo and a C compiler. This module is not cgo-free.

Default data directory:

- Unix: `~/.grape-core`
- Windows: `%AppData%\grape-core`

API keys are read from the environment named by `model.api_key_env`. They are not stored in the config file. `examples/basic` uses a mock model unless `GRAPE_REAL_MODEL=1`.

`workspace_write` runs commands through the tRPC managed OS sandbox (bubblewrap on Linux, Seatbelt on macOS). If that preflight fails, the command returns `agent.ErrSandboxUnavailable` and is not executed on the host. `danger_full_access` is explicit and reports isolation `none`. Windows has no managed native sandbox; that path returns `agent.ErrSandboxUnavailable`. macOS Seatbelt and Windows behavior are not verified by the Linux tests in this repository.

See [docs/design-spec.md](docs/design-spec.md) for the full requirements.

## Build

```bash
go build ./...
go test ./...
go vet ./...
```

```bash
go run ./examples/basic
```
