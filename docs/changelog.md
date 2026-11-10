# Changelog

## Unreleased

### Breaking

- `ForceReleaseSession` now takes `agent.SessionRelease` (`UserID`, and `AcceptConcurrentTools`). A call with only a session id no longer compiles. Without `AcceptConcurrentTools`, release is refused while a tool may still be running. Setting the flag does not stop that tool.
