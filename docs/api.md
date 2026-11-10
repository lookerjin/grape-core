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
    // permission_request, completed, failed, cancelled, or raw.
}
```

`RunHandle.Events` closes after one terminal event: `completed`, `failed`, or `cancelled`. `Sequence` is monotonic for that process-local run. It is not a durable cursor. `RunHandle.Close` drops unread events and releases the pump. Call it when the consumer will not read. After a run finishes, `context.event_hold_timeout` (default 30s) is how long the pump waits for a consumer who has not taken an event. `context.event_idle_timeout` (default 5m) is how long it waits after the last take. Either deadline drops the non-terminal backlog, leaves the terminal event, and exits. A reader who keeps taking events inside the idle window keeps every tool, permission, and error event. Cancelling the `Start` context does the same release. `Runtime.Close` releases every pump.

The stream keeps a soft buffer of 256 events. Only `text_delta` events are dropped when a consumer falls behind. One `dropped N events` marker is emitted per catch-up, not once per dropped delta. Tool calls, permission decisions, errors, hook failures, and the terminal event are not dropped while the consumer is still taking events; the buffer grows to hold them.

Tool authorization has one enforcement point: tRPC's per-run tool permission policy, after `BeforeTool` callbacks and before the tool runs. Read is granted only by a type check on grape's own read tools (builtin read tools, the private read wrapper, a read-level MCP tool grape built, and the tRPC skill load, list-docs, and select-docs types) and by `Options.NestedReads` agent/tool pairs. Names, descriptions, and `ToolMetadata` do not grant read. A wrapper that exposes some other tool through `Original` does not inherit that privilege.

`Options.Subagents` is the static subagent entry. Each value is a `SubagentSpec` (name, instruction, model, tools). Grape constructs the `llmagent` with the same execution settings as the root: code-execution processor off, knowledge-only skills or the executor grape selected, and the same tool kinds. Grape then wraps that agent with tRPC's `AgentTool`. `transfer_to_agent` is not registered. Framework tool calls on that child go through the run policy. A caller-built `agent.Agent` is not a spec. It is accepted only as `Options.TrustedAgents` or `ToolSpec.Trusted`, and grape does not apply those execution settings to it. A raw `*tool/agent.Tool` in `Tools` or `ToolSpecs` is rejected unless `Trusted`. `FixedAgent` is the same kind of caller-controlled bypass. An externally supplied `transfer.Tool` is a side effect. It is not allowed merely because of its type. A nested agent cannot take the root's place by naming itself `grape`.

`ToolSpec.Kind` `read` is the caller's own classification and grants read privilege. It is not inferred from the tool's name, description, or metadata.

`skills.knowledge_only` defaults to true and registers only the knowledge skill tools. Setting it false attaches tRPC's sandbox code executor. When `workspace.permission` or `sandbox.mode` is `danger_full_access`, grape passes tRPC's local code executor explicitly and reports isolation `none`. That is not managed isolation, and it is not an implicit fallback. Windows `managed` returns unavailable. Fenced code blocks in model replies are not executed on agents grape builds.

Plugin shell hooks do not run because the plugin was installed or enabled. `hooks.allow` lists component keys such as `name/hooks`. Hooks passed in `Options.Hooks` are an embedder grant. An allowed hook is not approved again on each callback. `BeforeTool` still runs before the tool policy, so an allowed hook can have side effects when the tool call is later denied. The sandbox still applies. Stdio MCP process launch stays on `trust: host` and is not part of the tool policy.

`context.max_llm_calls` (default 128) counts model calls for the whole run, including nested agents. `context.max_tool_iterations` (default 64) counts model turns that request tools, one round no matter how many calls run in parallel, also for the whole run. `context.max_tool_calls` counts individual executions and is unlimited when zero or negative. Exceeding a budget ends the run with terminal status `failed`.

`RunHandle.Cancel` cancels the caller context and the tRPC managed run. The session lock stays held until in-flight tool calls return. `context.tool_drain_timeout` (default 30s) bounds that wait. After it, the session is poisoned and `Start` returns that error until `ForceReleaseSession`. Releasing the lock does not stop a tool that ignores cancellation. `Enqueue` uses `SteerableRunner` when the runner supports it.

Approvals are `agent.ApprovalRequest` values delivered to an `agent.Decider`. The SDK does not read stdin. Plan mode asks before mutating tools. A denial does not call the tool.

Sessions are tRPC SQLite sessions. `ListSessions` and `DeleteSession` return grape-core structs, not tRPC sessions. Re-open the same database path to continue a session.

Model keys come from `model.api_key_env`. Gemini and Anthropic names are sent through the OpenAI-compatible client in tRPC v1.12.0. Hunyuan uses `secret_id_env` plus `api_key_env`. Set `GRAPE_REAL_MODEL=1` only when a real endpoint should be called. Tests use `internal/mockmodel` and do not need a key.

Errors wrap the sentinels in `agent`: `ErrInvalidConfig`, `ErrPermissionDenied`, `ErrSandboxUnavailable`, `ErrRunCancelled`, `ErrModel`, `ErrTool`, `ErrSession`, and `ErrPlugin`.
