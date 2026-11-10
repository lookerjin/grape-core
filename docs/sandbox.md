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

A managed command inherits the core shell environment (`PATH`, `HOME`, `LANG`, `TERM`, and the other startup variables), not the full host environment. Names that look like secrets are removed. `sandbox.env_allow` replaces that list. `HOME` inside the sandbox is the workspace home, not the host home.

`FilesystemIsolation` is false for this profile. trpc-agent-go v1.12.0 `WorkspaceWriteProfile` starts bubblewrap with `--ro-bind / /` and then adds a writable bind for the workspace grant. A managed command can still `cat` an ordinary file outside the workspace. Writes outside that grant are denied.

Existing sensitive paths are passed to `PermissionProfile.WithNoAccessPaths`, which the Linux backend enforces with deny-read masks (`tmpfs` mode `000` for directories, a mask file for regular files). `WithNoAccessGlobs` is used only for workspace-relative patterns in `sandbox.deny_read`; v1.12.0 rejects absolute globs. `Capabilities.SensitivePathsMasked` lists the paths that existed and were masked. `FilesystemIsolation` stays false because the rest of the host remains readable.

The default list, used when `sandbox.deny_read_mode` is `add`, is the following paths under the user home when they exist:

- `~/.ssh`
- `~/.gnupg`
- `~/.aws`
- `~/.azure`
- `~/.config/gcloud`
- `~/.kube`
- `~/.docker/config.json`
- `~/.netrc`
- `~/.git-credentials`
- `~/.config/gh`

It also includes the grape-core data directory (`~/.grape-core`, or `%AppData%\grape-core` on Windows) when that directory exists, the SQLite session file when it lives outside that directory and exists, and any `file:` credential path named by MCP `credential_ref` or headers. Missing paths are omitted. `sandbox.deny_read_mode: override` uses only `sandbox.deny_read`. Paths that contain the command workspace are omitted so the workspace grant is not covered by a mask. Bubblewrap scratch directories live in the system temp directory, outside the data directory, so the data directory itself can be masked.

`read_only` never starts a command. The managed profile builder still attaches the same no-access list to `ReadOnlyProfile` for that permission, and to `WorkspaceWriteProfile` for `workspace_write`.

`NetworkIsolation` is true when `sandbox.network` is `restricted` and the preflight succeeded. The Linux backend uses a fresh network namespace, so a connect to a listener on the host loopback is refused. `sandbox.network: enabled` sets `NetworkIsolation` false.

Managed mode on macOS is the tRPC Seatbelt path in upstream `codeexecutor/sandbox`. This repository's Linux VM did not execute Seatbelt. Treat macOS isolation as implemented by the pinned tRPC code and unverified here.

Windows has no managed native backend. `commandExecutor` returns `agent.ErrSandboxUnavailable` when `GOOS` is `windows` or when tests set `Options.Platform` to `windows`. That unit test runs on Linux and only checks the fail-closed branch. It is not a Windows execution. `host_windows.go` uses `CommandContext` for explicit `danger_full_access` and does not claim process-tree cleanup. Job Objects, WSL, and Docker are not labeled as a managed Windows sandbox. `sandbox.Backend` is the extension point for a future implementation that has passed the checks in design spec section 10.3.

`run_command` takes an argv, not a shell string. The tool result includes `isolation`: `managed`, `none`, or `external`. The tool permission policy decides whether the command may start. The executor enforces the sandbox again and does not ask for a second approval.

Skill execution uses the same choice when `skills.knowledge_only` is false. `workspace_write` with `managed` uses the tRPC OS sandbox. `danger_full_access`, either as `workspace.permission` or `sandbox.mode`, runs on the host and is reported as isolation `none`, not as managed. Windows `managed` returns unavailable and does not run on the host. External skill execution is refused rather than falling back to the host. Fenced code blocks are not an execution path.

Workspace file tools use `os.Root`. A symlink whose target is outside the workspace is rejected. An absolute symlink is also rejected, even when the target is inside the workspace. That is an `os.Root` limit, not a separate policy.

Stdio MCP, after `trust: host`, is started with a minimal environment on Linux (`env -i` plus `PATH`, `HOME`, `LANG`, `TERM`, and the other shell startup names). On Windows the upstream stdio client is started as given, so the server still inherits the host environment. That path was not executed here.

Git status and diff are fixed argv inspections. They are not the sandbox command path, and they do not stage files.
