# Plugins

A plugin is a directory, not a tRPC runner plugin and not a loaded `.so`.

```text
my-plugin/
├── .claude-plugin/plugin.json
├── skills/<name>/SKILL.md
├── commands/<name>.md
├── agents/<name>.md
├── rules/<name>.md
├── hooks/hooks.json
└── .mcp.json
```

`plugin.json` supports `name`, `version`, `description`, `author`, and `license`. Other JSON fields are ignored. They are not treated as aliases of those names.

`plugin.Manager` installs from a local directory or `InstallGit`. Git installs record `git rev-parse HEAD` and do not fetch on the next process start. Copies reject `..` paths and symlinks. A failed install removes its staging directory. Enable and disable change the install record used by the next runtime. A runtime keeps the snapshot it resolved at construction.

`.mcp.json` understands `mcpServers.<name>.command`, `args`, `url`, and `headers`. Header values may be `env:NAME`, `file:PATH`, or text containing `${NAME}` such as `Bearer ${TOKEN}`. A plugin server stays disabled until `mcp.allow_plugin_servers` lists `plugin/server`. A plugin cannot declare `trust: host`; the user grant does, as `{name: plugin/server, trust: host}`. Plugin headers never resolve `file:` refs. `env:` and `${NAME}` resolve only for names listed in `mcp.plugin_env_allow` under that `plugin/server` key. Stdio then runs on the host with isolation `none`. On Linux that process gets a minimal environment. On Windows the stdio environment is not minimized.

### Executable Markdown

The runtime snapshots enabled definitions after resolving conflicts. Use `rt.Definitions()` to obtain their owner, kind, name, description, instruction, requested tools and named arguments. The optional `plugins.enabled` list further filters manager-enabled installs. Plain Markdown supplies just the instruction. Optional YAML front matter is delimited by lines containing `---`. An explicit name must match the filename stem. Unknown fields, invalid names, duplicate or unused parameters and missing tool grants fail runtime construction. The size limit is 64 KiB per definition.

An `agents/analyst.md` can contain:

```markdown
---
description: Analyze the supplied evidence
model: inherit
tools: [archive]
---
Compare the evidence and save the report using archive.
```

Select it with `rt.Start(ctx, agent.RunRequest{AgentID: "analyst", UserID: u, SessionID: s, Message: prompt})`. The native named agent factory uses the runtime model, budgets, callbacks and permission policy. Plugins get no inherited coding tools, skills, executors, MCP or tool sets. Every requested tool needs a matching policy-checked grant in `Options.PluginTools["bundle/analyst"]` for its owning plugin. Even with a grant, the active permission mode and approval policy apply. Caller-built fixed agents and plugin agents cannot be combined. Plugins can omit `model` or select `inherit`.

A `commands/compare.md` can contain:

```markdown
---
description: Compare a named topic
arguments: [topic]
---
Compare the evidence for {{topic}}.
```

Call `rt.StartCommand(ctx, "compare", map[string]string{"topic": "latency"}, req)`. Leave `req.Message` empty. Optional attachments, output schema and agent selection work as for Start. Every declared argument must be present and nonblank, with no extra keys. Substitution matches literal `{{name}}` tokens in a single pass. The Claude-style whole-string `$ARGUMENTS` token accepts a single `"ARGUMENTS"` map entry and an optional `argument-hint` for display. It cannot be mixed with named arguments or positional indexing. Rendered commands are limited to 1 MiB. Values are ordinary prompt text. There is no shell evaluation or command scheduler. Enabled plugins with the same component name use the configured conflict policy. Disabling a plugin removes its entries on the next runtime construction.

`hooks.json` events that have a callback are `BeforeRun`, `AfterRun`, `BeforeModel`, `AfterModel`, `BeforeTool`, `AfterTool`, and `OnEvent`. `OnError` is unsupported. Installing or enabling the plugin does not run these hooks. `hooks.allow` must list the component key `name/hooks`. Hooks passed to runtime `Options.Hooks` are a separate embedder grant. An allowed hook is not sent through the tool approval decider on every call. Shell hooks still run through `sandbox.Executor`, not `os/exec`.

Skills are discovered once by grape's `skill.Discover`. That choice of directory is what `Get` and `Path` load, through one tRPC `FSRepository` rooted at the selected directory. A duplicate name follows `plugins.conflict` (`error`, `earliest`, or `latest`) and a `SKILL.md` that does not parse fails discovery. tRPC's own multi-root scan is different: it keeps the first name it sees and skips a bad file without an error. Grape does not use that scan to choose the directory. Doc filtering, recursion, sorting, and CRLF handling are tRPC's. `skills.knowledge_only` defaults to true, which selects the knowledge-only tool profile and does not attach a code executor. Setting it false attaches tRPC's sandbox executor, or tRPC's local executor when the user explicitly chose `danger_full_access`. Isolation for that local executor is `none`. The runtime always sets `WithEnableCodeExecutionResponseProcessor(false)`, so a fenced code block in a model reply is not executed.

Rules load from the global data directory, then plugin files, then `<workspace>/.grape/rules`, sorted inside each root. The runtime appends the applicable text to the agent instruction.
