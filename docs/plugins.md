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

`.mcp.json` understands `mcpServers.<name>.command`, `args`, `url`, and `headers`. Header values may be `env:NAME`, `file:PATH`, or `${NAME}`. A plugin server stays disabled until `mcp.allow_plugin_servers` lists `plugin/server`. Stdio servers also need `trust: host` and then run on the host with isolation `none`.

`commands/*.md` and `agents/*.md` are discovery-only. The runtime lists them on the plugin snapshot (`discovery_only`) and does not turn them into tools or subagents.

`hooks.json` events that have a callback are `BeforeRun`, `AfterRun`, `BeforeModel`, `AfterModel`, `BeforeTool`, `AfterTool`, and `OnEvent`. `OnError` is unsupported. Shell hooks run through `sandbox.Executor`, not `os/exec`.

Skills are discovered as `SKILL.md` summaries. The runtime passes those directories to tRPC `skill.NewFSRepository` with `SkillToolProfileKnowledgeOnly` unless knowledge-only mode is turned off, and it always sets `WithEnableCodeExecutionResponseProcessor(false)`.

Rules load from the global data directory, then plugin files, then `<workspace>/.grape/rules`, sorted inside each root. The runtime appends the applicable text to the agent instruction.
