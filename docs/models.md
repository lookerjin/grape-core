# Model infrastructure

The SDK has three independent entry points. `model/catalog` serves discovery data. `model.Registry` owns account-scoped connections, credentials and model selections. `runtime/trpc` binds a selection to one native Agent Run. Products can open any of these without an account or network access.

## Connect a product

The host authenticates the user and supplies trusted application configuration. Create a registry scoped to that principal, keep it open for the Runtime's lifetime, then close the Runtime before the registry. Separate processes may open the same registry database. Choose an absolute path in a directory controlled by the application. Any process with arbitrary filesystem or native code access to a user's account is outside this boundary.

```go
scope := model.Scope{App: "my-desktop", User: authenticatedUser}
registry, err := model.Open(model.Options{Scope: scope, Database: absoluteDBPath})
if err != nil { return err }
defer registry.Close()

// Run this step when the user explicitly creates a connection.
conn, err := registry.AddEnv(ctx, model.Provider{
    ID: "openai", Protocol: model.Chat, Endpoint: "https://api.openai.com/v1",
}, "Personal", "MY_OPENAI_KEY")
if err != nil { return err }

rt, err := trpcruntime.New(trpcruntime.Options{
    AppName: scope.App, ModelRegistry: registry, DataDir: appDataDir,
})
if err != nil { return err }
defer rt.Close()

ref := model.Ref{ConnectionID: conn.ID, ModelID: selectedModel}
handle, err := rt.Start(ctx, agent.RunRequest{
    UserID: scope.User, SessionID: sessionID, ModelRef: &ref, Message: prompt,
})
if err != nil { return err }
for event := range handle.Events() { consume(event) }
```

* `AddKey` stores a direct API key in the supplied `model.Store`. The default is the operating system keychain. `AddEnv` stores only an environment-variable name. A headless host can explicitly supply `&model.MemoryStore{}` for ephemeral credentials or its own secure store. Use the same backend after restart. A keychain error is surfaced to the caller.
* `List`, `SetEnabled`, `Remove`, `CredentialStatus` and `SetDefault` act on the registry's principal. Changing an endpoint or account requires creating a connection with a distinct ID. Authenticate the principal independently of `RunRequest.UserID`. A registry-bound Runtime verifies both app and user before resolving a selection.
* `SetDefault` persists the selection for subsequent runs. Explicit `ModelRef` overrides it. Selecting both `ModelName` and `ModelRef` is an error. An installed registry never falls back to an environment-configured account when resolution fails. TaskHost snapshots the default on submission. Core-built plugin agents and ordinary subagents inherit the native model override. Explicit subagent models stay pinned.
* Without a registry, the single-provider config and injected model entry points continue to work. `New` can open the SQLite session, inspect tools and serve management operations before login. A model-dependent `Start` reports `model.ErrNotConfigured`, `model.ErrCredentialUnavailable` or `model.ErrReauth` as applicable. No placeholder API key or process-wide environment change is needed.
* `agent.RunInspector.ContextStatus()` includes the actual root model and connection ID once the root request is projected. `RunStatus.ModelError` classifies recognized registry-backed terminal HTTP failures, including authentication, rate limits, explicit quota exhaustion and model-not-found responses. Provider error bodies are removed before native error reporting.
* An uploaded file reference must carry its owning `Attachment.ConnectionID` in a registry run. Inline bytes have no provider account binding. Session history and `ReadAttachment` preserve the owner. When a different connection projects that file the model call fails before transport. Returning to the original connection restores access while the provider file exists. To use another account independently, attach appropriate files in a fresh session. Core does not upload or transfer provider files.

## Catalog and availability

`catalog.Open` loads the last validated cache or an embedded eight-provider snapshot. It never starts a network request. Use `List()` and `Status()` immediately. Run `Refresh(ctx)` in a product-owned goroutine for asynchronous updates. It returns the new status or an error. The status digest identifies the accepted content version. Failure retains the previous snapshot.

Supply an application-controlled `SourcesPath` to replace the embedded YAML. Each enabled source has an ID, priority, format and either an absolute file path or an HTTPS URL. A positive timeout bounds each HTTP attempt. `max_bytes` uses bytes. The loader rejects redirects. Explicit `AllowLoopbackHTTP` is limited to numeric loopback addresses. `Offline` disables every HTTP source. Set `TTL` to skip refreshes inside a freshness window. HTTP validators, bounded retries for transient status codes, content digests and atomic cache replacement protect the previous catalog. A bad source configuration is reported in `Status.Warning`. Trusted custom parsers and model overrides are accepted through `catalog.Options`; an override is a complete record, including its own unknown fields.

Models.dev pricing is published per million tokens. Optional capability flags distinguish unknown from false. Published metadata and endpoint hints do not establish account entitlement. `model.BuiltinProviders()` supplies separately maintained inference destinations. Custom provider IDs and model IDs need no public catalog entry. Do not construct a credential-bearing endpoint from untrusted discovery data or workspace settings.

`registry.Discover(ctx, connectionID)` queries the selected Chat Completions connection's authenticated models endpoint. Advertised models initially have unknown availability. `registry.Models(ctx, connectionID)` reads durable, timestamped observations offline. A successful completed inference provides positive evidence. Only a model-specific not-found response records unavailability. These observations are historical and may go stale.

The embedded snapshot was selected from the Models.dev API on 2026-11-11. The selected provider and model keys are present in `model/catalog/snapshot.json`. To regenerate, select those same keys from the configured source and retain the identity, modalities, capabilities, limits, costs and update dates. Upstream licensing accompanies the snapshot in `model/catalog/LICENSE.models-dev`.

## OAuth

Register the consuming application with the provider. For Hugging Face, configure `model.HuggingFace(clientID, clientSecret)` in `model.Options.OAuth`. Supply only the credentials appropriate to that application's registration. An application that cannot keep a client secret must use a registration permitted for its client type. The adapter uses the account's authenticated userinfo subject and requests the inference scope. Core verifies the signed ID token's issuer, audience, expiry and subject. Browser callbacks must also match the pending nonce. The authenticated router uses Chat Completions. Catalog providers cannot change these OAuth destinations.

1. Call `BeginLogin(ctx, providerID, registeredLoopbackURL)` or `BeginDevice(ctx, providerID)`. The PKCE flow owns its loopback listener. Port zero selects an available port when permitted by the provider's registration. The product presents `login.Challenge.URL` and the device user code when present.
2. Call `login.Complete(label)` to await login, or `Cancel()` to abandon it. This returns the connection only after an authenticated userinfo response identifies the account. Reauthorization of the same subject under the same registration retains its connection ID. Distinct registrations or accounts receive distinct connections.
3. Use `Discover` or explicitly select a model. Starting an OAuth background task or enabling automatic Evolution with OAuth also requires `AllowBackground(ctx, connectionID, true)` following the user's consent. Inference refreshes tokens at the request boundary. A SQLite write transaction coordinates rotation across registry handles using the same database and store. Rejected refresh grants require user interaction.
4. `Remove` removes the local connection and secret. The user can revoke the application at the provider. Hugging Face's discovery document does not advertise a token revocation endpoint.

| Path | Verification |
| --- | --- |
| API-key chat, Anthropic Messages, Gemini | Native tRPC protocol adapters. Deterministic local HTTP tests. |
| OAuth PKCE, device authorization, scoped discovery, refresh, streaming and tool calls | Local OAuth and inference servers exercise actual HTTP exchanges, the tRPC runner and a real workspace tool. |
| Hugging Face production OAuth | Official OAuth and inference endpoints configured. Real user consent, subscription entitlements and production model calls require an application registration and eligible account. |
| Sign in with ChatGPT / Responses API | No production adapter in this release. It requires a separate registration and Responses inference contract. |

### Headless example

Run the example from the Core checkout. `GRAPE_MODEL_DATA` can select an absolute local directory.

```sh
go run ./examples/models catalog
go run ./examples/models refresh
go run ./examples/models add-env https://api.openai.com/v1 OPENAI_API_KEY MODEL_ID
go run ./examples/models run 'List the workspace files using the file tool.'
```

For OAuth, set the registered application's `HF_CLIENT_ID` and, when applicable, `HF_CLIENT_SECRET` locally. Run `go run ./examples/models device MODEL_ID`, then repeat the same `run` command in a new process. Browser-based PKCE uses `login MODEL_ID` and an exact registered loopback URI in `HF_CALLBACK`. Use `accounts` to inspect saved connections and `logout CONNECTION_ID` to remove one. The example uses the system keychain for durable login.
