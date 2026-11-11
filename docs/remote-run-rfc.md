# RFC: Resuming runs from multiple clients

**Status** — proposal only. No durable replay API ships in this change.

A product can recover conversation history and inspect TaskHost work today. Neither source can reproduce every public Run event. The optional NDJSON Journal truncates and redacts fields, has no transactional cursor, and is a debug export. Event.Sequence is local to a run. TaskHost persists task lifecycle but cannot reconstruct a pending approval or an abandoned event stream.

## Proposed contract

| Proposed operation | Consistency requirement |
| --- | --- |
| `Snapshot(ctx, Scope, RunID)` | Return current execution status, last committed cursor, delivery status and outstanding approvals in one revision. Authenticate the principal at the gateway and verify app, user and session at lookup. |
| `Subscribe(ctx, Scope, RunID, afterCursor)` | Deliver retained events after an exclusive cursor, at least once. Clients deduplicate by run and durable sequence. A retention gap returns a typed cursor-expired result and requires a fresh snapshot. |
| `ResolveApproval(ctx, Scope, ApprovalID, expectedRevision, decision, requestID)` | Exactly one successful compare-and-swap from pending. A repeated identical request returns the recorded answer. A different answer conflicts. Permission narrowing and expiry still apply. |
| `CancelRun(ctx, Scope, RunID, requestID)` | An idempotent request. Only the execution owner assigns a terminal outcome. A disconnected viewer cannot cancel implicitly. |

Use a durable sequence allocated alongside the authoritative state update. Persist approval claims before admitting the side effect. A crash between side-effect execution and acknowledgment can leave its physical outcome unknown. Never automatically replay non-idempotent tools. A recovered live task whose execution owner vanished should be marked interrupted, with its committed prefix available for inspection.

The tRPC Session remains the source of conversation history. TaskHost can supply restart classification. Neither can supply an atomic append-and-status contract for all public events today. Prototype that contract only when a remote product specifies retention, storage limits, authority delegation and latency. Keep WebSockets, user login and remote transport outside Core.

Before implementation, validate disconnect during approval, competing decisions, cursor expiry, cancellation racing terminal completion, and restart after a side effect but before acknowledgment.
