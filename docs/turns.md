# Turn controls and recovery

Public entry points are on `runtime/trpc.Runtime` and use the SDK's `agent`, `mcp`, and `model` packages. The offline executable in [examples/recovery](../examples/recovery) opens a connection, runs a real save tool, forks the conversation, previews a restore, detects an intervening edit and reads a recovery backup.

## Cancellation and per-turn controls

`Cancel()` requests cancellation. Until execution reaches its terminal boundary, `ForceReleaseSession` returns a session error if no drain exists. Once a terminal status becomes visible, Core has either released the session or installed the drain for tools already executing. A finished run declines newly arriving tracked calls. The rules cover Core's callable and stream wrappers and managed MCP tools. Opaque third-party work outside these calls is outside the drain contract.

Use `rt.ForceReleaseSession(id, agent.SessionRelease{UserID: user, RunID: handle.ID()})` for owner-checked release. A busy tool also requires `AcceptConcurrentTools: true`. This explicitly allows concurrent side effects. A replacement run has its own lock ownership.

`agent.RunRequest.Reasoning` is optional. Select one of `Effort` or positive `BudgetTokens`. For example, set `Reasoning: &agent.ReasoningOptions{Effort: "high"}` on one request. The selected native model receives the patch for that turn. Omission preserves provider defaults. Task submission copies the options. Use `errors.Is(err, agent.ErrInvalidConfig)` for local rejection.

| Selected native adapter | Controls mapped to the wire |
| --- | --- |
| Chat Completions | Low, medium or high effort. Custom model support is unknown until the provider accepts the request. The DeepSeek protocol profile accepts high or max. |
| Anthropic Messages | For the named Claude Opus 4.6/4.7/4.8 and Sonnet 4.6 models, adaptive thinking and effort. Sonnet/Haiku/Opus 4.5 support a fixed budget of at least 1,024 and below maximum output. Other aliases are unverified. |
| Gemini | Gemini 3 family low/high levels, Gemini 2.5 positive fixed budgets. The model server enforces model-specific limits. |
| Injected/fixed agent | Explicit reasoning controls are rejected. |

`ModelInfo.ReasoningEffortSupport` and `ThinkingBudgetSupport` distinguish supported, unsupported and unknown adapter/model combinations. Unknown on a Chat Completions endpoint allows an explicit wire request and the provider can reject it. Use `trpcruntime.ConnectionModelInfo(connection, modelID)` with a connection from the registry to inspect these fields offline. They do not establish entitlement.

When available, `Event.Usage` carries provider totals and positive cache-read, cache-write and reasoning counts. Its source is `provider`. Upstream represents absent and exact-zero details identically, so a missing detail stays nil. Use `RunInspector.ContextStatus().EstimatedMessageTokens` (via the returned status) for the native local rune-based estimate of messages and tool calls. Tool schemas and binary content are excluded. Byte fields remain byte counts.

## Conversation boundaries

Set `trpcruntime.Options{TurnCheckpoints: true}` with a private SQLite session database. Before a new session's first validated Run enters the Runner, Core persists its native empty-conversation state as a baseline. Successful turns then record their native state and event/track high-water marks. This requires the pinned SQLite v1.12.0 schema and synchronous persistence. Only one Runtime may write a particular session at a time. The session lock coordinates runs within that Runtime. Separately opened processes can read persisted data after shutdown.

| Operation | API and behavior |
| --- | --- |
| List boundaries | `rt.Checkpoints(ctx, user, session)` returns an optional baseline followed by completed Run IDs in order. The baseline has `Baseline == true` and `RunID == agent.SessionBaselineID`. Pass that selection key to the existing fork, preview and rewind methods. It has no synthetic completed Run or event. Existing history without markers cannot be selected. |
| Fork | `rt.ForkSession(ctx, user, session, runID)` returns a new ID. A transaction copies the native event and track prefix, attachment references and session state. The fork has a source marker. Subsequent turns are independent. Files, artifacts and global user state keep their original scopes. |
| Side question | `rt.AskSide(ctx, user, session, question, modelRef)` reads the last completed prefix and issues one tool-free model call. Nil selects the default. The result carries its own usage and checkpoint. The main lock, events, history and budget remain independent. The currently supported projection is the grape root conversation, up to 16 MiB of selected native events. |
| Rewind conversation | `rt.RewindSession(ctx, user, session, runID)` restores state and truncates newer native events, tracks and checkpoint markers in a single transaction. Cached summaries are invalidated. The completed conversation recovery record commits in the same transaction. |

Give these APIs the principal authenticated by the host. All selections include the Runtime's app and the supplied user and session. Registry-bound runtimes also validate the user against the registry principal. Fork during a run can select an earlier completed marker. Rewind obeys the normal session lock and caller deadline.

The `Baseline` field is an additive API extension. Method signatures and completed-turn IDs are unchanged. Products that display only completed Runs should filter the field. A fresh fork of a baseline remains empty and selectable. A fork of a completed turn preserves that turn's source marker.

Existing nonempty sessions keep their actual history and checkpoints. Enabling checkpoints on such a session does not add a fabricated baseline. Only an empty session without historical events, tracked changes or checkpoints can acquire an original baseline. There is no database rewrite or attempt to reconstruct old filesystem contents.

A cancelled or failed Run does not create a completed boundary. Managed changes from that Run still participate in the restore plan relative to the last successful boundary or baseline. A recorded pending write has an unknown physical outcome and causes a conflict, even if the bytes happen to match its intended content.

### Three recovery modes

| Requested behavior | Calls and independent results |
| --- | --- |
| Conversation | Call `RewindSession` with the selected key. File conflicts do not block it. It uses the session lock and a SQLite transaction. |
| Code | Preview, confirm the digest, then call `RestoreFiles`. Conversation history and checkpoint selections remain intact. |
| Conversation and code | Call both APIs and retain both outcomes. Report partial success when one fails. `Recoveries` returns separate records. |

For the combined choice, a product can perform the calls below after confirming the file preview. Handle both errors. A failed file restore does not establish the conversation result.

```go
files, fileErr := rt.RestoreFiles(ctx, user, session, checkpoint.RunID, preview.Digest)
conversationErr := rt.RewindSession(ctx, user, session, checkpoint.RunID)
records, queryErr := rt.Recoveries(ctx, user, session)
// Present files.Status, fileErr, conversationErr and records to the user.
// Keep the operation ID for RecoveryBackups and RecoveryBackup.
```

## File recovery

Checkpoint mode records before-images for `save_file` and `replace_content`. Recording is opt-in because workspace contents can be sensitive. It captures regular files and absence, text bytes and Unix permission bits. Each image is limited to 1 MiB. A session has 32 MiB of encoded mutation images and 64 MiB of encoded recovery backups. `rt.RecoveryLimits()` reports these fixed caps. `errors.Is(err, agent.ErrCheckpointLimit)` identifies a managed-write or backup capacity failure. The caps stay fixed in this release because the current product has no demonstrated requirement to enlarge them. Writes that cannot be checkpointed fail before mutation. Shell, Git, hooks, external editors and MCP mutations are not attributed to these tools. Directories created along the way, ACLs, hardlink topology and extended attributes are outside file restore.

1. Call `rt.PreviewFiles(ctx, user, session, checkpointID)`. Show all paths, actions and conflicts. The target count is `len(plan.Files)`. `TrackingScope` lists the represented write tools. An empty plan does not establish that Shell, MCP or external writes never happened. `PreviewFile(ctx, user, session, checkpointID, root, path)` returns current and proposed images for a diff. An intervening edit, an interrupted write, a revoked workspace grant or a broken chain of versions produces a conflict.
2. Confirm the exact preview with the user. Pass its digest to `rt.RestoreFiles(ctx, user, session, checkpointID, digest)`. A stale digest or any preflight conflict refuses the batch. A read-only Runtime rejects file restoration. Current workspace and extra-root grants bound eligible paths.
3. Each path gets a durable backup before modification and another version check immediately before rename or removal. File replacement uses a temporary file inside the confined destination directory. An independent editor can still write between check and replacement. Stronger guarantees require an actually exclusive or isolated workspace.
4. Inspect `Recoveries(ctx, user, session)` for independent conversation/files results. After a partial failure, take a fresh preview and confirm the remaining paths. `RecoveryBackups(ctx, user, session, operationID)` lists backup paths and status. `RecoveryBackup(ctx, user, session, operationID, root, path)` retrieves one image. A crash can leave a pending operation. If a prepared backup proves the intended image was already installed, the next confirmed preview can finalize its record without overwriting the file. Other mismatches remain conflicts.

Images reject final-path symlinks and nonregular files. All path operations stay within `os.Root`. Intermediate symlinks must resolve inside the granted root. A replaced path is checked again, including its permission bits. Atomic rename replaces the path's inode, so hardlinked peers keep their own bytes. ACLs, ownership, extended attributes and directory topology are not restored. Removal or retargeting of an extra-root grant can make a recorded path unavailable.

Checkpoints and backup data live with the session until deletion or native session expiry cleanup. Foreign keys and the soft-delete trigger remove their dependent records when the owning session is removed. `DeleteSession` also takes the session lock. Storage caps bound image growth without pruning recoverable chains. Underlying database pages and separate database backups follow SQLite and host retention policies. The session database is created or restricted to owner-only permissions when checkpoints are enabled. Embedding products control OS-account isolation, storage encryption and database backup retention.

## Live MCP

Trusted product code can call `SetMCP(ctx, mcp.Server{...})` to add or reload a definition, `RestartMCP(ctx, name)` to reconnect the same definition, and `RemoveMCP(ctx, name)` to retire it. These are runtime-local operations. Persist desired startup configuration in the host. `MCPStatus()` includes the generation of each installed definition and discovered tool names. Failed replacement returns an error and keeps the prior connection. Untrusted stdio is rejected before spawn. Plugin definitions cannot be overwritten through SetMCP.

A run snapshots the published sets. New tools become visible to subsequent runs. After removal, a previously obtained tool handle rejects new calls. A call already inside the transport can complete and its final release closes the retired connection. Existing Grape approvals, permission levels and redaction apply.

## Subscription providers

A trusted OAuth registration can supply `WithInferenceRoutes(model.InferenceRoutes{Generate: "/tenant/infer", Models: "/account/models", DecodeModels: decoder})`. Both paths are fixed suffixes on the registered inference origin. Codecs receive a bounded catalog body, without credentials. The token transport uses the registered method and path. This separates catalog shape, native inference protocol and token lifecycle. The checked-in fixture exercises PKCE/device grants, renewal, account discovery, streaming, effort and real tool calls against custom routes.

Hugging Face remains the built-in OAuth provider. No new production subscription account was verified. GitHub Models has retired. The documented Copilot SDK runs its own tool loop in the Copilot CLI. Embedding that engine would change Core's execution boundary. Sign in with ChatGPT is deferred by product decision.

[GitHub Models status](https://docs.github.com/en/github-models) · [Copilot agent loop](https://docs.github.com/en/copilot/how-tos/copilot-sdk/features/agent-loop)

## Rewind verification

The deterministic tests use real files and SQLite plus the pinned native Agent Runner. They cover first-turn success, cancellation after save, provider failure after save, a failed replacement after save, subprocess exit after save, restart recovery, empty baseline fork, legacy history without a fabricated baseline, continued runs after rewind, pending-write refusal, expired previews, backup failure, a second path changed to a directory by an independent writer during recovery, partial retry and session deletion cleanup.

Run from the Core module with `GOWORK=off`.

```sh
go test ./... -count=1
go test -race ./runtime/trpc ./coding -count=1
go vet ./...
go build -o /tmp/grape-recovery ./examples/recovery
/tmp/grape-recovery
```

The recovery executable cancels its first Run after its real managed save, closes and reopens the Runtime, forks the persisted baseline, rejects an expired preview, restores the original file, reads the backup and independently rewinds the conversation.

CLI integration was verified using unchanged source at `a8b926ebcdc58a6e7f230f436059617e0a04aedf`, built against the local Core with an external Go workspace. The actual TUI approved a save, selected the baseline and confirmed rewind. The file returned to its original contents and the native conversation became empty, with separate completed file and conversation records. That CLI version exposes a combined restore action. Its UI still needs product-side integration for the three choices and the baseline label.

Real commercial-provider inference and strict cross-editor atomic recovery were not tested. The local HTTP fixture verifies Core behavior without establishing provider entitlement.
