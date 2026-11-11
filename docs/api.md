# API

Import the runtime as `trpcruntime`.

```go
cfg, err := config.Load(configFile)
rt, err := trpcruntime.New(trpcruntime.Options{Config: &cfg})
defer rt.Close()

run, err := rt.Start(ctx, agent.RunRequest{
    UserID: "local-user",
    SessionID: "demo",
    Workspace: "/path/to/repo",
    Message: "fix the failing test",
})
for evt := range run.Events() {
    // evt.Kind is text_delta, text, tool_start, tool_end,
    // permission_request, artifact, structured_output, completed, failed, cancelled, or raw.
}
```

An active consumer reads one terminal event. Ordinary events have process-local sequence numbers. A dropped-delta marker is a delivery diagnostic without a sequence number. The stream has no replay cursor.

`Status` reports execution and delivery separately. Execution stays `pending` until the runner produces its result. `Close` abandons unread events and requests cancellation if execution is still pending. A status of `pending` with delivery `abandoned` is valid. Cancellation caused by Close is reported as `cause: delivery` when execution settles. A terminal event already placed in the output channel counts as delivered. User cancellation uses `Cancel` or the Start context. After completion, a blocked stream send is bounded by `context.event_hold_timeout` (default 30s) before the first observed read, and `context.event_idle_timeout` (default 5m) after reader progress. A reader outside those bounds can use `Status` to inspect the result. Runtime.Close releases outstanding pumps.

The stream keeps a soft buffer of 256 events. Only `text_delta` events are dropped when a consumer falls behind. One `dropped N events` marker is emitted per catch-up, not once per dropped delta. Tool calls, permission decisions, errors, hook failures, and the terminal event are not dropped while the consumer is still taking events; the buffer grows to hold them.

Tool authorization has one enforcement point: tRPC's per-run tool permission policy, after `BeforeTool` callbacks and before the tool runs. Read is granted only by a type check on grape's own read tools (static tools classified as read, a read-level MCP tool grape built, and the tRPC skill load, list-docs, and select-docs types) and by `Options.NestedReads` agent/tool pairs. Names, descriptions, and `ToolMetadata` do not grant read. A wrapper that exposes some other tool through `Original` does not inherit that privilege.

`Options.Subagents` is the static subagent entry. Each value is a `SubagentSpec` (name, instruction, model, tools). Grape constructs the `llmagent` with the same execution settings as the root: code-execution processor off, knowledge-only skills or the executor grape selected, and the same tool kinds. Grape then wraps that agent with tRPC's `AgentTool`. `transfer_to_agent` is not registered. Static tools on that child use the run policy. External `SubagentSpec.ToolSets` require `TrustToolSets: true`, including when refreshed on each run. This grants trust to provider discovery, the tools it returns, and any nested agents. v1.12.0 evaluates a provider tool's own permission checker before the run policy. A provider that returns an allowing decision, launches a process during discovery, or performs work internally owns that boundary. Configure stdio MCP through the managed MCP configuration when host-launch grants must apply. A caller-built `agent.Agent` is not a spec. It is accepted only as `Options.TrustedAgents` or `ToolSpec.Trusted`, and grape does not apply those execution settings to it. A raw `*tool/agent.Tool` in `Tools` or `ToolSpecs` is rejected unless `Trusted`. `FixedAgent` is the same kind of caller-controlled bypass. An externally supplied `transfer.Tool` is a side effect. It is not allowed merely because of its type. A nested agent cannot take the root's place by naming itself `grape`.

`ToolSpec.Kind` `read` is the caller's own classification and grants read privilege. It is not inferred from the tool's name, description, or metadata. Unknown kinds and untrusted static tools without `Call` are rejected during construction. Set `workspace.disable_coding_tools: true` to omit filesystem, command and Git tools on the root. The default preserves the original Coding surface.

`skills.knowledge_only` defaults to true and registers only the knowledge skill tools. Setting it false attaches tRPC's sandbox code executor. When `workspace.permission` or `sandbox.mode` is `danger_full_access`, grape passes tRPC's local code executor explicitly and reports isolation `none`. That is not managed isolation, and it is not an implicit fallback. Windows `managed` returns unavailable. Fenced code blocks in model replies are not executed on agents grape builds.

Plugin shell hooks do not run because the plugin was installed or enabled. `hooks.allow` lists component keys such as `name/hooks`. Hooks passed in `Options.Hooks` are an embedder grant. An allowed hook is not approved again on each callback. `BeforeTool` still runs before the tool policy, so an allowed hook can have side effects when the tool call is later denied. The sandbox still applies. Stdio MCP process launch stays on `trust: host` and is not part of the tool policy.

`context.max_llm_calls` is the per-agent model-call limit passed to tRPC. Nil defaults to 128. Zero or negative is unlimited. tRPC resets that counter for each agent invocation. `context.max_tool_iterations` is the per-agent tool-round limit, one round however many calls run in parallel. Nil defaults to 64. Zero or negative is unlimited. `context.total_max_llm_calls` (nil defaults to 128) and `context.total_max_tool_iterations` (nil defaults to 64) are one run-level counter across nested agents. tRPC does not provide that total. `context.max_tool_calls` counts attempts, including calls later denied or short-circuited, and is unlimited when nil, zero, or negative. Exhausting it ends the run with `budget_exceeded` and `scope` `run` before the next model call. `max_llm_calls` and `max_tool_iterations` used to bound the whole run, and 0 selected the default. They are now per agent, and 0 means unlimited. A root agent stopped at its own limit ends `failed` with `reason` `budget_exceeded` and `scope` `agent` when grape's own count has reached that limit, and `agent_id` set. A root `stop_agent_error` that the count does not confirm is `failed` with `reason` `agent_stopped`. The run counter ends `failed` with `reason` `budget_exceeded` and `scope` `run`. A child that hits its own limit does not fail a run the root then completes. The terminal reason is not chosen by matching error text.

`RunHandle.Cancel` cancels the caller context and the tRPC managed run. The session lock is keyed by app, user, and session, so two users with the same session id do not block each other. The lock covers the call lifetime of static root and spec tools, configured MCP callable adapters, and a wrapped stream until its reader finishes or closes. Runtime-added tRPC skill/extension tools, external ToolSets, FixedAgent internals and work a tool detaches do not have this drain guarantee. Cancelling a streaming Trusted `AgentTool` can release the session before the child finishes: tRPC closes that stream writer when the parent context is done. `context.tool_drain_timeout` (default 30s) bounds that wait. For streamable HTTP, Grape uses the POST response stream for tool results and disables the optional background GET notification listener, which races with discovery in trpc-mcp-go v0.0.20. This covers a configured MCP call only until its **client** returns. A cancelled HTTP request can return locally while its server handler continues. A trusted child ToolSet can likewise outlive the child stream, though cancellation can also leave the session draining until the child tool returns. Both cases allow the next turn in the same session to complete before external work stops. For tracked calls still running beyond the drain timeout, the session is poisoned and `Start` returns that error until `ForceReleaseSession`. `ForceReleaseSession` refuses while a tool may still be running unless `AcceptConcurrentTools` is set. That flag is not safe recovery: it does not stop the tool. `RunRequest.AgentID` is passed to tRPC `WithAgentByName`. An unknown name is an error. An empty id uses the default agent. `Enqueue` uses `SteerableRunner` when the runner supports it.

Approvals are `agent.ApprovalRequest` values delivered to an `agent.Decider`. The SDK does not read stdin. Plan mode asks before mutating tools. A denial does not call the tool.

Sessions are tRPC SQLite sessions. `ListSessions` and `DeleteSession` return grape-core structs, not tRPC sessions. Re-open the same database path to continue a session.

Use `rt.SessionHistory(ctx, userID, sessionID, eventLimit)` (or `agent.SessionHistoryReader`) to read the stored conversation in chronological order. The runtime scopes the query to its app name and the supplied user. A missing session returns `agent.ErrSession`. A positive limit selects recent persisted events before projecting messages, so the returned message count can differ. Zero uses the SQLite service limit. The projection includes text and tool calls/results, masks configured secrets, and includes attachment metadata and references, and omits streaming partials, runner metadata, reasoning and raw attachment bytes.

Model context settings apply to the root and Core-built child agents. `context.max_history_messages` maps to tRPC's `WithMaxHistoryRuns`, which counts projected messages and can drop orphaned tool results at the boundary. Zero keeps the available history. `context.compaction: true` activates native prompt-side compaction for oversized historical tool results. Add a positive `context.oversized_tool_result_tokens` to also shorten oversized current results. By default compaction is off. Neither setting rewrites the stored conversation or starts a summarization model call.

Model keys come from `model.api_key_env`. `gemini` selects the Gemini Developer API and `anthropic` selects Anthropic Messages via their native v1.12.0 tRPC model modules. A custom `model.base_url` must serve the selected provider's wire protocol. For existing OpenAI-compatible gateways serving Gemini or Claude, set `model.provider: openai_compatible` and keep the gateway URL and model name. An injected model takes precedence over provider construction. Hunyuan uses `secret_id_env` plus `api_key_env`. Set `GRAPE_REAL_MODEL=1` only when a real endpoint should be called. Tests use `internal/mockmodel` and do not need a key.

Errors wrap the sentinels in `agent`: `ErrInvalidConfig`, `ErrPermissionDenied`, `ErrSandboxUnavailable`, `ErrRunCancelled`, `ErrModel`, `ErrTool`, `ErrSession`, and `ErrPlugin`.



### Attachments, structured responses and artifacts

An embedding application can pass the following request to `rt.Start`, or pass it to `StartCommand` with an empty Message. Read `handle.Events()` through closure and check `handle.Status()`. `agent.EventStructured` carries the validated JSON value directly in `event.Payload`. A tool save emits `agent.EventArtifact` with name, version, size and MIME.

```go
req := agent.RunRequest{
    UserID: userID, SessionID: sessionID, AgentID: "analyst",
    Message: "Describe this image and the attached report.",
    Attachments: []agent.Attachment{
        {Kind: "image", MIME: "image/png", Data: pngBytes},
        {Kind: "file", Name: "report.pdf", MIME: "application/pdf", FileID: providerFileID},
    },
    Output: &agent.OutputSchema{Name: "report", Strict: true, Schema: json.RawMessage(
        `{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"],"additionalProperties":false}`)},
}
handle, err := rt.Start(ctx, req)
```

* Attach inline PNG, JPEG, GIF or WebP images, or named files. Limits per turn are 16 attachments, 8 MiB per inline part and 16 MiB total. Inline image MIME must match the bytes. Native image parts have no display name. The SDK accepts data supplied by the caller and OpenAI-style uploaded file IDs (`file-...`). Paths and arbitrary URLs are outside this input protocol. The SDK opens no user-supplied path and fetches no URL.
* SQLite retains the original input with the user event. `SessionHistory` returns attachment metadata and scoped references. `rt.ReadAttachment(ctx, userID, sessionID, ref)` reads the single anchor event and returns bytes or the original provider file ID. Inline data survives a runtime restart with the same database until its event expires or the session is deleted. Provider IDs retain the provider's account and expiry rules. The SDK does not download or renew them.
* Built-in multipart adapters are OpenAI, OpenAI-compatible, Gemini and Anthropic. Provider IDs are supported by the OpenAI protocol. Anthropic inline documents are restricted to PDF and plain text. Known provider-specific variants selected by the adapter URL also use their restricted capability set. The remote model must accept the requested content and MIME. For injected models, the embedder supplies the capabilities. Input errors are returned by Start and endpoint rejections are reported by the run.
* For structured output the runtime passes the object or array schema to tRPC's per-run JSON Schema option. Tools keep their normal execution path. OpenAI, compatible endpoints and Gemini have native schema request fields. Specific models and gateways can differ in enforcement. Strict is forwarded to the adapter. Grape validates the upstream parsed value locally before emitting the structured event. An unrecovered terminal provider API or stream error fails the run, including when the native runner drops that error from its completion event. A missing or invalid value fails an otherwise successful run. Cancellation and budget stops retain their own terminal reason. Schemas have a 64 KiB limit, support local references, and cannot load external schemas. tRPC parses untyped numbers as float64. Represent high-precision identifiers as strings.
* Inject the caller-owned native backend in `trpcruntime.Options{ArtifactService: service}`. The service can be in-memory, S3, COS or another implementation. `store, err := rt.Artifacts(userID, sessionID)` binds the scope to the runtime app and those IDs. Authenticate the user before supplying these IDs. Use `store.Save(ctx, name, mime, data)`, `store.List(ctx)`, `store.Versions(ctx, name)`, `store.Read(ctx, name, &version)` (nil selects latest) and `store.Delete(ctx, name)` (all revisions). A registered tool uses `trpcruntime.ArtifactsFromContext(ctx)` or native callback artifact methods on its invocation. Installation does not expose a model-facing storage tool.
* Names and scope IDs must be single path segments. The native shared user namespace and reserved session ID `user` are unavailable through this API. The embedding application owns deletion and backend shutdown. Deleting a conversation does not delete its separately stored artifacts. In-memory content lives with its service instance. A new runtime using the same persistent backend, app, user and session can read pinned revisions. Concurrent writers to one S3 artifact must coordinate externally, since the upstream S3 backend allocates versions by listing existing revisions.

### Current capability matrix

| Surface | Status | Execution contract |
| --- | --- | --- |
| Core runs, Graph, sessions, steering | Implemented | Native Runner, Invocation, SQLite, Graph and ToolCallID plugin. Headless callers consume the same RunHandle. |
| Coding and Research | Implemented | Both examples use Options and Start. Research selects knowledge-only Skills and a read tool with Coding tools disabled. |
| Static subagents and trusted extensions | Implemented | Spec children use native AgentTool. Caller-built agents and external ToolSet providers require an explicit trust decision. |
| Plan and Goal | Implemented | Plan gates mutations. Goal exposes native get/create/update tools. An active objective uses the native retry limit; complete and blocked are persisted goal states. Run completion alone does not assert that the objective is complete. |
| Goal promise marker | Unsupported | The old default marker is accepted for config compatibility. Custom markers previously had no effect and now fail validation. Use update_goal to change objective status. |
| TaskHost | Implemented | One host per runtime. Repeated construction shares it. Closing stops new work and cancels active work. On restart, unfinished records become interrupted. Use a dedicated data directory per active task writer. Native in-process TaskRun v1.12.0 calls Runner.Run directly and defaults to a separate child session. Its RunOptions and RunContext extension points can carry policy and state explicitly, but direct delegation omits Grape's per-run state, permission policy, same-session lock and drain handling. TaskHost continues to enter through Runtime.Start. Cross-process ownership and automatic resume are unsupported. |
| Skills, Rules, MCP, Hooks | Implemented | Rules feed instructions. Knowledge-only is the default. Executable Skills require a supported selected executor. Config grants gate plugin hooks and MCP launch. Fenced model output never selects an executor. |
| Plugin agents and commands | Implemented | Definitions from enabled plugins feed named runner factories and explicit StartCommand calls. See [plugin definitions](plugins.md). |
| Memory and Evolution | Implemented, opt-in | Markdown recall and tRPC Evolution with a human gate. The embedder can supply a reviewer. |
| Sandbox | Platform dependent | Managed commands require a successful preflight including exit status. Windows managed execution and external Skill executors are unsupported. See [sandbox boundaries](sandbox.md). |
| Product interfaces | Outside the SDK | TUI, Desktop, Web and service policies are independent consumers. |
