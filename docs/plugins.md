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

`commands/*.md` and `agents/*.md` are discovery-only. The runtime lists them on the plugin snapshot (`discovery_only`) and does not turn them into tools or subagents.

`hooks.json` events that have a callback are `BeforeRun`, `AfterRun`, `BeforeModel`, `AfterModel`, `BeforeTool`, `AfterTool`, and `OnEvent`. `OnError` is unsupported. Installing or enabling the plugin does not run these hooks. `hooks.allow` must list the component key `name/hooks`. Hooks passed to runtime `Options.Hooks` are a separate embedder grant. An allowed hook is not sent through the tool approval decider on every call. Shell hooks still run through `sandbox.Executor`, not `os/exec`.

Skills are discovered as `SKILL.md` summaries. The runtime passes those directories to tRPC `skill.NewFSRepository`. `skills.knowledge_only` defaults to true, which selects the knowledge-only tool profile and does not attach a code executor. Setting it false attaches tRPC's sandbox executor, or tRPC's local executor when the user explicitly chose `danger_full_access`. Isolation for that local executor is `none`. The runtime always sets `WithEnableCodeExecutionResponseProcessor(false)`, so a fenced code block in a model reply is not executed.

Rules load from the global data directory, then plugin files, then `<workspace>/.grape/rules`, sorted inside each root. The runtime appends the applicable text to the agent instruction.
