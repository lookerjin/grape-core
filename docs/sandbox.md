# Sandbox

`workspace.permission` and `sandbox.mode` together decide how a command runs.

| permission | sandbox.mode | result |
| --- | --- | --- |
| `read_only` | any | command returns `ErrPermissionDenied` and does not start |
| `workspace_write` | `managed` | tRPC OS sandbox after a real preflight |
| `workspace_write` | `danger_full_access` | host process, isolation `none` |
| `danger_full_access` | any | host process, isolation `none` |
| any write mode | `external` | the named `sandbox.Backend` after `Healthy` |

Managed mode on Linux uses tRPC bubblewrap. The preflight runs `/bin/true` inside the sandbox. If bubblewrap is missing or the preflight fails, `Execute` returns `agent.ErrSandboxUnavailable` and does not run the command on the host.

`FilesystemIsolation` is false for this profile. trpc-agent-go v1.12.0 `WorkspaceWriteProfile` starts bubblewrap with `--ro-bind / /` and then adds a writable bind for the workspace grant. A managed command can `cat` a file outside the workspace. Writes outside that grant are denied. grape-core reports that split in `Capabilities.Reason` instead of claiming host reads are blocked.

`NetworkIsolation` is true when `sandbox.network` is `restricted` and the preflight succeeded. The Linux backend uses a fresh network namespace, so a connect to a listener on the host loopback is refused. `sandbox.network: enabled` sets `NetworkIsolation` false.

Managed mode on macOS is the tRPC Seatbelt path in upstream `codeexecutor/sandbox`. This repository's Linux VM did not execute Seatbelt. Treat macOS isolation as implemented by the pinned tRPC code and unverified here.

Windows has no managed native backend. `commandExecutor` returns `agent.ErrSandboxUnavailable` when `GOOS` is `windows` or when tests set `Options.Platform` to `windows`. That unit test runs on Linux and only checks the fail-closed branch. It is not a Windows execution. `host_windows.go` uses `CommandContext` for explicit `danger_full_access` and does not claim process-tree cleanup. Job Objects, WSL, and Docker are not labeled as a managed Windows sandbox. `sandbox.Backend` is the extension point for a future implementation that has passed the checks in design spec section 10.3.

`run_command` takes an argv, not a shell string. The tool result includes `isolation`: `managed`, `none`, or `external`.

Git status and diff are fixed argv inspections. They are not the sandbox command path, and they do not stage files.
