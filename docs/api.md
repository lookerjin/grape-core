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

Nested tool calls are authorized by the calling invocation, not by the tool's name or description. Read is granted only to the read-tool instances grape constructed, to a user-configured MCP server whose `level` is `read`, and to `Options.NestedReads` agent/tool pairs. A wrapper that exposes some other tool through `Original` does not inherit that privilege. A nested agent cannot take the root's place by naming itself `grape`.

`context.max_llm_calls` (default 128) counts model calls for the whole run, including nested agents. `context.max_tool_iterations` (default 64) counts model turns that request tools, one round no matter how many calls run in parallel, also for the whole run. `context.max_tool_calls` counts individual executions and is unlimited when zero or negative. Exceeding a budget ends the run with terminal status `failed`.

`RunHandle.Cancel` cancels the caller context and the tRPC managed run. The session lock stays held until in-flight tool calls return. `context.tool_drain_timeout` (default 30s) bounds that wait. After it, the session is poisoned and `Start` returns that error until `ForceReleaseSession`. Releasing the lock does not stop a tool that ignores cancellation. `Enqueue` uses `SteerableRunner` when the runner supports it.

Approvals are `agent.ApprovalRequest` values delivered to an `agent.Decider`. The SDK does not read stdin. Plan mode asks before mutating tools. A denial does not call the tool.

Sessions are tRPC SQLite sessions. `ListSessions` and `DeleteSession` return grape-core structs, not tRPC sessions. Re-open the same database path to continue a session.

Model keys come from `model.api_key_env`. Gemini and Anthropic names are sent through the OpenAI-compatible client in tRPC v1.12.0. Hunyuan uses `secret_id_env` plus `api_key_env`. Set `GRAPE_REAL_MODEL=1` only when a real endpoint should be called. Tests use `internal/mockmodel` and do not need a key.

Errors wrap the sentinels in `agent`: `ErrInvalidConfig`, `ErrPermissionDenied`, `ErrSandboxUnavailable`, `ErrRunCancelled`, `ErrModel`, `ErrTool`, `ErrSession`, and `ErrPlugin`.
