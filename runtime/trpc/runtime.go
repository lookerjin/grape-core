package trpcruntime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/config"
	"github.com/lookerjin/grape-core/hooks"
	"github.com/lookerjin/grape-core/internal/journal"
	"github.com/lookerjin/grape-core/internal/paths"
	"github.com/lookerjin/grape-core/mcp"
	"github.com/lookerjin/grape-core/memory"
	"github.com/lookerjin/grape-core/plugin"
	"github.com/lookerjin/grape-core/rules"
	"github.com/lookerjin/grape-core/sandbox"
	trpcagent "trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/agent/extension/goal"
	"trpc.group/trpc-go/trpc-agent-go/agent/llmagent"

	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/evolution"
	"trpc.group/trpc-go/trpc-agent-go/model"
	toolcallid "trpc.group/trpc-go/trpc-agent-go/plugin/toolcallid"
	"trpc.group/trpc-go/trpc-agent-go/runner"
	"trpc.group/trpc-go/trpc-agent-go/session"
	sqlitesession "trpc.group/trpc-go/trpc-agent-go/session/sqlite"
	trpcskill "trpc.group/trpc-go/trpc-agent-go/skill"
	"trpc.group/trpc-go/trpc-agent-go/tool"
	agenttool "trpc.group/trpc-go/trpc-agent-go/tool/agent"
	"trpc.group/trpc-go/trpc-agent-go/tool/function"

	_ "github.com/mattn/go-sqlite3"
)

const defaultAppName = "grape-core"

// SubagentSpec is a child agent grape constructs. It is not a
// caller-built tRPC agent. Tools are classified the same way as the
// root's tools. Nested Subagents are also built by grape and exposed
// as agent tools, not as transfer_to_agent.
type SubagentSpec struct {
	Name        string
	Instruction string
	Model       model.Model
	Tools       []tool.Tool
	ToolSpecs   []ToolSpec
	// TrustToolSets authorizes provider discovery and provider-owned tools, including their own permission checkers.
	TrustToolSets   bool
	ToolSets        []tool.ToolSet
	RefreshToolSets bool
	Subagents       []SubagentSpec
}

// Options configures a Runtime. Model, when set, replaces the configured
// provider. Tests and the offline example inject a mock model here.
type Options struct {
	Config                  *config.Config
	Model                   model.Model
	DataDir                 string
	AppName                 string
	Tools                   []tool.Tool
	Decider                 agent.Decider
	RejectConcurrentSession bool
	Instruction             string
	// Platform overrides GOOS for sandbox preflight. Leave it empty in
	// production. Tests set "windows" to exercise the fail-closed path.
	Platform      string
	External      sandbox.Backend
	PluginManager *plugin.Manager
	// FixedAgent replaces the grape agent. The caller controls that
	// agent's tools, executor, and any calls it makes itself. The run
	// policy is still attached for framework-scheduled tool calls.
	// Grape does not promise those internal calls stay inside the policy
	// or the sandbox.
	FixedAgent trpcagent.Agent
	// Subagents are specs grape compiles into llmagent values. Each one
	// gets the same execution settings as the root: the code-execution
	// processor is off, skills are knowledge-only or use the executor
	// grape selected, and tools are classified by grape. A caller-built
	// agent.Agent is not accepted here.
	Subagents []SubagentSpec
	// TrustedAgents are caller-built agents. Grape does not rebuild
	// them, so their processor, executor, and tool kinds stay theirs.
	// This is an explicit bypass, not the static subagent entry.
	TrustedAgents []trpcagent.Agent
	// ToolSpecs classifies extra tools. An empty Kind is side_effect.
	// Kind "read" is the caller's own classification and grants read.
	// It is not inferred from the tool. Options.Tools are untrusted
	// side effects. Trusted skips the run policy. A raw
	// *tool/agent.Tool is rejected unless Trusted.
	ToolSpecs []ToolSpec
	// Hooks are shell hooks the embedder has already authorized.
	// Plugin hooks stay off until config hooks.allow lists their key.
	Hooks             []hooks.Spec
	EvolutionReviewer evolution.Reviewer
	// NestedReads are agent/tool pairs a nested caller may run as read.
	// Privilege comes from this list, not from the tool's name or description.
	NestedReads []NestedRead
}

// Runtime is a tRPC-backed agent.Runtime.
//
// It owns the SQLite session service it opens. Close releases that database
// and the runner. A caller-supplied model is not closed.
type Runtime struct {
	cfg          config.Config
	appName      string
	runner       runner.Runner
	sessions     *sqlitesession.Service
	counter      *countingModel
	model        model.Model
	exec         *commandExecutor
	tools        []tool.Tool
	specs        []ToolSpec
	skillRoots   []string
	ruleText     string
	ruleSources  []rules.Source
	globalRules  string
	hookRunner   *hooks.Runner
	pluginHooks  []hooks.Spec
	mcpServers   []mcp.Server
	resolved     plugin.Resolved
	fixed        trpcagent.Agent
	subagents    []SubagentSpec
	trustedTools []tool.Tool
	evolution    evolution.Service
	mcpSets      []tool.ToolSet
	mcpStatus    []mcp.Status
	decider      agent.Decider
	grantedHooks []hooks.Spec
	reject       bool
	journal      *journal.Writer
	secrets      []string
	maxField     int
	instruction  string
	nested       map[string]map[string]struct{}

	mu    sync.Mutex
	locks map[sessionKey]*countedLock
	// holds is the in-flight drain for a session, keyed so an older run
	// cannot release or poison the run that replaced it.
	holds    map[sessionKey]*sessionHold
	active   map[*runHandle]struct{}
	taskHost *TaskHost
	starting sync.WaitGroup
	skills   trpcskill.Repository

	closed    bool
	closeOnce sync.Once
	closeErr  error
}

// New validates options, opens SQLite, and builds a tRPC runner.
func New(opts Options) (*Runtime, error) {
	cfg := config.Default()
	if opts.Config != nil {
		cfg = config.Normalize(*opts.Config)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := rejectReservedAgents(opts.FixedAgent, opts.TrustedAgents); err != nil {
		return nil, err
	}
	dataDir := opts.DataDir
	if dataDir == "" {
		var err error
		dataDir, err = paths.DataDir()
		if err != nil {
			return nil, agent.Wrap(agent.ErrInvalidConfig, "data dir", err)
		}
	}
	dataDir, err := paths.Expand(dataDir)
	if err != nil {
		return nil, agent.Wrap(agent.ErrInvalidConfig, "data dir", err)
	}
	cfg, err = resolvePaths(cfg, dataDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Session.Path), 0o755); err != nil {
		return nil, agent.Wrap(agent.ErrSession, "mkdir", err)
	}
	db, err := sql.Open("sqlite3", cfg.Session.Path+"?_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		return nil, agent.Wrap(agent.ErrSession, "open sqlite", err)
	}
	db.SetMaxOpenConns(1)
	sessOpts := []sqlitesession.ServiceOpt{sqlitesession.WithEnableAsyncPersist(false)}
	if cfg.Session.EventLimit > 0 {
		sessOpts = append(sessOpts, sqlitesession.WithSessionEventLimit(cfg.Session.EventLimit))
	}
	sessions, err := sqlitesession.NewService(db, sessOpts...)
	if err != nil {
		_ = db.Close()
		return nil, agent.Wrap(agent.ErrSession, "sqlite", err)
	}
	mdl, counter, err := buildModel(cfg, opts.Model)
	if err != nil {
		_ = sessions.Close()
		return nil, err
	}
	instruction := opts.Instruction
	if instruction == "" {
		instruction = "You are grape-core, a local assistant. Use tools only when they are provided. Never claim that a command ran unless a tool result says so. Do not stage, commit, push, reset, or clean git state."
	}
	appName := opts.AppName
	if appName == "" {
		appName = defaultAppName
	}
	rt := &Runtime{
		cfg:          cfg,
		appName:      appName,
		sessions:     sessions,
		counter:      counter,
		model:        mdl,
		exec:         newCommandExecutor(cfg, filepath.Join(os.TempDir(), "grape-core-sandbox"), dataDir, opts.Platform, opts.External),
		tools:        append([]tool.Tool(nil), opts.Tools...),
		specs:        append([]ToolSpec(nil), opts.ToolSpecs...),
		decider:      opts.Decider,
		grantedHooks: append([]hooks.Spec(nil), opts.Hooks...),
		reject:       opts.RejectConcurrentSession,
		secrets:      secretValues(cfg),
		maxField:     cfg.Journal.MaxFieldBytes,
		instruction:  instruction,
		locks:        map[sessionKey]*countedLock{},
		holds:        map[sessionKey]*sessionHold{},
		fixed:        opts.FixedAgent,
		subagents:    cloneSpecs(opts.Subagents),
		nested:       nestedReadIndex(opts.NestedReads),
	}
	if err := rt.prepareSurface(opts.TrustedAgents); err != nil {
		_ = sessions.Close()
		return nil, err
	}
	if cfg.Evolution.Enabled {
		if err := os.MkdirAll(cfg.Evolution.Root, 0o755); err != nil {
			_ = sessions.Close()
			return nil, err
		}
		eopts := []evolution.Option{
			evolution.WithManagedSkillsDir(filepath.Join(cfg.Evolution.Root, "skills")),
			evolution.WithCandidateStore(evolution.NewFileCandidateStore(filepath.Join(cfg.Evolution.Root, "candidates"))),
			evolution.WithHumanGate(evolution.NewAlwaysHoldGate()),
		}
		if opts.EvolutionReviewer != nil {
			eopts = append(eopts, evolution.WithReviewer(opts.EvolutionReviewer))
		}
		rt.evolution = evolution.NewService(mdl, eopts...)
	}
	if err := rt.collectExtensions(opts.PluginManager, dataDir); err != nil {
		_ = sessions.Close()
		return nil, err
	}
	runnerOpts := []runner.Option{
		runner.WithSessionService(sessions),
		// toolcallid canonicalizes tool call ids. llmflow already fills an
		// empty response id before these callbacks run.
		runner.WithPlugins(&hookPlugin{rt: rt}, toolcallid.New()),
	}
	if rt.evolution != nil {
		runnerOpts = append(runnerOpts, runner.WithEvolutionService(rt.evolution))
	}
	var r runner.Runner
	if rt.fixed != nil {
		r = runner.NewRunner(appName, rt.fixed, runnerOpts...)
	} else {
		r = runner.NewRunnerWithAgentFactory(appName, "grape", rt.agentFactory, runnerOpts...)
	}
	rt.runner = r
	if err := rt.openMCP(context.Background()); err != nil {
		_ = r.Close()
		_ = sessions.Close()
		return nil, err
	}
	if cfg.Journal.Enabled {
		jw, err := journal.Open(cfg.Journal.Path, cfg.Journal.MaxFieldBytes, rt.secrets)
		if err != nil {
			_ = r.Close()
			_ = sessions.Close()
			for _, set := range rt.mcpSets {
				_ = set.Close()
			}
			return nil, agent.Wrap(agent.ErrInvalidConfig, "journal", err)
		}
		rt.journal = jw
	}
	return rt, nil
}

func (rt *Runtime) agentFactory(ctx context.Context, ro trpcagent.RunOptions) (trpcagent.Agent, error) {
	ws := rt.cfg.Workspace.Root
	if ro.RuntimeState != nil {
		if v, ok := ro.RuntimeState["workspace"].(string); ok && v != "" {
			ws = v
		}
	}
	allowWrite := rt.cfg.Workspace.Permission != agent.PermissionReadOnly
	var tools []tool.Tool
	if !rt.cfg.Workspace.DisableCodingTools {
		tools = rt.workspaceTools(ws, allowWrite)
	}
	for _, spec := range rt.specs {
		if spec.Tool == nil {
			continue
		}
		tools = append(tools, spec.Tool)
	}
	tools = append(tools, rt.tools...)
	mode := ""
	if ro.RuntimeState != nil {
		mode, _ = ro.RuntimeState["mode"].(string)
	}
	for _, spec := range rt.subagents {
		child, err := rt.buildSubagent(spec, ws)
		if err != nil {
			return nil, err
		}
		tools = append(tools, child)
	}
	tools = append(tools, rt.trustedTools...)
	if rt.cfg.Memory.Enabled {
		tools = append(tools, rt.memoryTool())
	}
	instruction := rt.instruction
	if text, err := rt.ruleTextFor(ws); err == nil && text != "" {
		instruction += "\n\nRules:\n" + text
	}
	opts, err := rt.executionOptions(ws, instruction, rt.model, tools)
	if err != nil {
		return nil, err
	}
	if len(rt.mcpSets) > 0 {
		opts = append(opts, llmagent.WithToolSets(rt.mcpSets))
	}
	if mode == string(agent.ModeGoal) {
		retries := rt.cfg.Goal.MaxIterations
		opts = append(opts, llmagent.WithExtensions(goal.New(goal.WithMaxRetries(retries))))
	}
	// Spec subagents are tRPC AgentTools. WithSubAgents would also
	// register transfer_to_agent, which this runtime does not expose.
	return llmagent.New(rootAgentName, opts...), nil
}

func (rt *Runtime) executionOptions(ws, instruction string, mdl model.Model, tools []tool.Tool) ([]llmagent.Option, error) {
	if mdl == nil {
		mdl = rt.model
	}
	opts := []llmagent.Option{
		llmagent.WithModel(mdl),
		llmagent.WithInstruction(instruction),
		llmagent.WithGenerationConfig(generationConfig(rt.cfg)),
		llmagent.WithMaxHistoryRuns(rt.cfg.Context.MaxHistoryMessages),
		llmagent.WithEnableContextCompaction(rt.cfg.Context.Compaction),
		llmagent.WithContextCompactionOversizedToolResultMaxTokens(rt.cfg.Context.OversizedToolResultTokens),
		// The framework default executes fenced code blocks without the
		// tool permission policy. Keep it off on every agent grape builds.
		llmagent.WithEnableCodeExecutionResponseProcessor(false),
		llmagent.WithWorkspaceExecSurfaceEnabled(false),
		llmagent.WithMaxLLMCalls(positiveLimit(rt.cfg.Context.MaxLLMCalls)),
		llmagent.WithMaxToolIterations(positiveLimit(rt.cfg.Context.MaxToolIterations)),
		llmagent.WithTools(tools),
	}
	repo, err := rt.skillRepository()
	if err != nil {
		return nil, err
	}
	if repo == nil {
		return opts, nil
	}
	opts = append(opts, llmagent.WithSkills(repo))
	if rt.cfg.KnowledgeOnly() {
		opts = append(opts, llmagent.WithSkillToolProfile(llmagent.SkillToolProfileKnowledgeOnly))
		return opts, nil
	}
	exec, _, err := rt.exec.skillCodeExecutor(ws)
	if err != nil {
		return nil, err
	}
	opts = append(opts,
		llmagent.WithCodeExecutor(exec),
		llmagent.WithSkillToolProfile(llmagent.SkillToolProfileFull),
	)
	return opts, nil
}

func (rt *Runtime) buildSubagent(spec SubagentSpec, ws string) (tool.Tool, error) {
	var tools []tool.Tool
	tools = append(tools, spec.Tools...)
	for _, item := range spec.ToolSpecs {
		if item.Tool != nil {
			tools = append(tools, item.Tool)
		}
	}
	for _, child := range spec.Subagents {
		nested, err := rt.buildSubagent(child, ws)
		if err != nil {
			return nil, err
		}
		tools = append(tools, nested)
	}
	opts, err := rt.executionOptions(ws, spec.Instruction, spec.Model, tools)
	if err != nil {
		return nil, err
	}
	if len(spec.ToolSets) > 0 {
		opts = append(opts, llmagent.WithToolSets(spec.ToolSets))
	}
	if spec.RefreshToolSets {
		opts = append(opts, llmagent.WithRefreshToolSetsOnRun(true))
	}
	ag := llmagent.New(spec.Name, opts...)
	return kindTool(agenttool.NewTool(ag), "side_effect"), nil
}

func (rt *Runtime) prepareSurface(trusted []trpcagent.Agent) error {
	if err := prepareTools(rt.tools, rt.specs); err != nil {
		return err
	}
	if err := prepareSpecs(rt.subagents); err != nil {
		return err
	}
	wrapped := make([]tool.Tool, 0, len(trusted))
	for _, ag := range trusted {
		if ag == nil {
			continue
		}
		wrapped = append(wrapped, wrapTrusted(agenttool.NewTool(ag)))
	}
	rt.trustedTools = wrapped
	return nil
}

func prepareSpecs(specs []SubagentSpec) error {
	for i := range specs {
		if len(specs[i].ToolSets) > 0 && !specs[i].TrustToolSets {
			return agent.Invalid("subagent.tool_sets", fmt.Errorf("provider code requires TrustToolSets"))
		}
		if strings.TrimSpace(specs[i].Name) == "" {
			return agent.Invalid("subagent", fmt.Errorf("name is required"))
		}
		if specs[i].Name == rootAgentName {
			return agent.Wrap(agent.ErrInvalidConfig, "agent", fmt.Errorf("non-root agent named %q is reserved", rootAgentName))
		}
		if err := prepareTools(specs[i].Tools, specs[i].ToolSpecs); err != nil {
			return err
		}
		if err := prepareSpecs(specs[i].Subagents); err != nil {
			return err
		}
	}
	return nil
}

// New owns these slices via cloneSpecs and the copies in Runtime.
func prepareTools(raw []tool.Tool, specs []ToolSpec) error {
	for i, item := range raw {
		if err := rejectRawAgentTool(item); err != nil {
			return err
		}
		raw[i] = kindTool(item, "side_effect")
	}
	for i, spec := range specs {
		if spec.Tool == nil {
			continue
		}
		if spec.Trusted {
			if err := rejectReservedTool(spec.Tool); err != nil {
				return err
			}
			specs[i].Tool = wrapTrusted(spec.Tool)
			continue
		}
		if err := rejectRawAgentTool(spec.Tool); err != nil {
			return err
		}
		switch spec.Kind {
		case "", "read", "write", "command", "side_effect":
		default:
			return agent.Invalid("tool.kind", fmt.Errorf("unknown kind %q", spec.Kind))
		}
		kind := spec.Kind
		if kind == "" {
			kind = "side_effect"
		}
		specs[i].Tool = kindTool(spec.Tool, kind)
	}
	return nil
}

func cloneSpecs(in []SubagentSpec) []SubagentSpec {
	if len(in) == 0 {
		return nil
	}
	out := make([]SubagentSpec, len(in))
	for i, spec := range in {
		spec.Tools = append([]tool.Tool(nil), spec.Tools...)
		spec.ToolSpecs = append([]ToolSpec(nil), spec.ToolSpecs...)
		spec.ToolSets = append([]tool.ToolSet(nil), spec.ToolSets...)
		spec.Subagents = cloneSpecs(spec.Subagents)
		out[i] = spec
	}
	return out
}

func (rt *Runtime) memoryTool() tool.Tool {
	return rt.asRead(function.NewFunctionTool(
		func(ctx context.Context, in struct {
			Query string `json:"query"`
		}) ([]memory.Entry, error) {
			store, err := memory.Open(rt.cfg.Memory.Root)
			if err != nil {
				return nil, err
			}
			return store.Recall(in.Query)
		},
		function.WithName("memory_recall"),
		function.WithDescription("Search Markdown memory notes. This does not write memories or call a model."),
	))
}

// EvolutionEnabled reports whether an evolution service was constructed.
func (rt *Runtime) EvolutionEnabled() bool { return rt.evolution != nil }

func resolvePaths(cfg config.Config, dataDir string) (config.Config, error) {
	var err error
	if cfg.Session.Path == "" {
		cfg.Session.Path = filepath.Join(dataDir, "session.db")
	}
	if cfg.Session.Path, err = paths.Expand(cfg.Session.Path); err != nil {
		return cfg, agent.Wrap(agent.ErrInvalidConfig, "session.path", err)
	}
	if cfg.Workspace.Root == "" {
		cfg.Workspace.Root = "."
	}
	if cfg.Workspace.Root, err = paths.Expand(cfg.Workspace.Root); err != nil {
		return cfg, agent.Wrap(agent.ErrInvalidConfig, "workspace.root", err)
	}
	if cfg.Memory.Root == "" {
		cfg.Memory.Root = filepath.Join(dataDir, "memory")
	}
	if cfg.Memory.Root, err = paths.Expand(cfg.Memory.Root); err != nil {
		return cfg, agent.Wrap(agent.ErrInvalidConfig, "memory.root", err)
	}
	if cfg.Evolution.Root == "" {
		cfg.Evolution.Root = filepath.Join(dataDir, "evolution")
	}
	if cfg.Evolution.Root, err = paths.Expand(cfg.Evolution.Root); err != nil {
		return cfg, agent.Wrap(agent.ErrInvalidConfig, "evolution.root", err)
	}
	if cfg.Journal.Path == "" {
		cfg.Journal.Path = filepath.Join(dataDir, "journal.ndjson")
	}
	if cfg.Journal.Path, err = paths.Expand(cfg.Journal.Path); err != nil {
		return cfg, agent.Wrap(agent.ErrInvalidConfig, "journal.path", err)
	}
	return cfg, nil
}

func secretValues(cfg config.Config) []string {
	var out []string
	if cfg.Model.APIKeyEnv != "" {
		if v := os.Getenv(cfg.Model.APIKeyEnv); v != "" {
			out = append(out, v)
		}
	}
	if cfg.Model.SecretIDEnv != "" {
		if v := os.Getenv(cfg.Model.SecretIDEnv); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// Config returns the resolved configuration.
func (rt *Runtime) Config() config.Config { return rt.cfg }

// ModelCalls reports how many times the wrapped model was invoked.
func (rt *Runtime) ModelCalls() int64 {
	if rt.counter == nil {
		return 0
	}
	return rt.counter.calls.Load()
}

// Start implements agent.Runtime.
//
// The caller context bounds the run. Cancel on the handle aborts it as well.
func (rt *Runtime) Start(ctx context.Context, req agent.RunRequest) (agent.RunHandle, error) {
	if rt == nil || rt.runner == nil {
		return nil, agent.Wrap(agent.ErrInvalidConfig, "start", fmt.Errorf("runtime is closed"))
	}
	if err := ctx.Err(); err != nil {
		return nil, agent.Wrap(agent.ErrRunCancelled, "start", err)
	}
	if req.UserID == "" || req.SessionID == "" {
		return nil, agent.Invalid("run", fmt.Errorf("user_id and session_id are required"))
	}
	if req.Message == "" {
		return nil, agent.Invalid("run", fmt.Errorf("message is required"))
	}
	if req.Mode == "" {
		req.Mode = agent.ModeDefault
	}
	switch req.Mode {
	case agent.ModeDefault, agent.ModePlan, agent.ModeGoal:
	default:
		return nil, agent.Invalid("mode", fmt.Errorf("unsupported mode %q", req.Mode))
	}
	unlock, err := rt.lockSession(ctx, req.UserID, req.SessionID)
	if err != nil {
		return nil, err
	}
	rt.mu.Lock()
	if rt.closed {
		rt.mu.Unlock()
		unlock()
		return nil, agent.Invalid("start", fmt.Errorf("runtime is closed"))
	}
	rt.starting.Add(1)
	rt.mu.Unlock()
	defer rt.starting.Done()
	workspace := req.Workspace
	if workspace == "" {
		workspace = rt.cfg.Workspace.Root
	}
	workspace, err = filepath.Abs(workspace)
	if err != nil {
		unlock()
		return nil, agent.Invalid("workspace", err)
	}
	runID := uuid.NewString()
	runCtx, cancel := context.WithCancel(ctx)
	st := newRunState(runID, req.SessionID, req.Mode)
	runCtx = withState(runCtx, st)
	runCtx = context.WithValue(runCtx, workspaceKey{}, workspace)
	runOpts := []trpcagent.RunOption{
		trpcagent.WithRequestID(runID),
		trpcagent.WithToolPermissionPolicyFunc(rt.toolPermission),
		trpcagent.WithRuntimeState(map[string]any{
			"workspace": workspace,
			"mode":      string(req.Mode),
		}),
	}
	if req.AgentID != "" {
		// tRPC resolves the name. An unknown name is an error from the
		// runner. Grape does not keep its own agent registry.
		runOpts = append(runOpts, trpcagent.WithAgentByName(req.AgentID))
	}
	events, err := rt.runner.Run(
		runCtx,
		req.UserID,
		req.SessionID,
		model.NewUserMessage(req.Message),
		runOpts...,
	)
	if err != nil {
		cancel()
		unlock()
		return nil, classifyRunError(err)
	}
	box := newEventBox(256)
	ch := make(chan agent.Event, 3)
	h := &runHandle{
		id:         runID,
		events:     ch,
		box:        box,
		stop:       make(chan struct{}),
		resultDone: make(chan struct{}),
		cancel: func() error {
			cancel()
			if managed, ok := rt.runner.(runner.ManagedRunner); ok {
				managed.Cancel(runID)
			}
			return nil
		},
		enqueue: func(message string) error {
			return runner.EnqueueUserMessage(rt.runner, runID, model.NewUserMessage(message))
		},
	}
	rt.track(h)
	go func() {
		if box.pump(ch, h.stop, rt.eventHoldTimeout(), rt.eventIdleTimeout()) {
			h.noteStreamDelivered()
		} else {
			h.noteDeliveryClosed()
		}
		close(ch)
		rt.untrack(h)
	}()
	go func() { defer cancel(); rt.forward(runCtx, req, runID, st, events, box, h, unlock) }()
	return h, nil
}

func classifyRunError(err error) error {
	if err == nil {
		return nil
	}
	return agent.Wrap(agent.ErrModel, "run", err)
}

func (rt *Runtime) forward(ctx context.Context, req agent.RunRequest, runID string, st *runState, in <-chan *event.Event, out *eventBox, h *runHandle, unlock func()) {
	defer rt.finishSession(req.UserID, req.SessionID, runID, st, unlock)
	defer out.close()
	conv := newConverter(runID, req.SessionID, rt.maxField, rt.secrets)
	terminal := false
	emit := func(evt agent.Event) {
		if evt.RunID == "" {
			evt.RunID = runID
		}
		if evt.SessionID == "" {
			evt.SessionID = req.SessionID
		}
		evt = applyStop(st, evt)
		if evt.Kind == agent.EventCompleted && ctx.Err() != nil {
			evt.Kind = agent.EventCancelled
			evt.Payload = agent.MustPayload(agent.TerminalPayload{
				Status: string(agent.EventCancelled),
				Error:  ctx.Err().Error(),
			})
		}
		if evt.Terminal() {
			if h != nil {
				h.noteResult(evt, ctx.Err() == context.Canceled)
			}
			rt.finishSession(req.UserID, req.SessionID, runID, st, unlock)
		}
		if rt.journal != nil {
			_ = rt.journal.Append(evt)
		}
		out.push(evt)
		if evt.Terminal() {
			terminal = true
		}
	}
	flush := func() {
		for _, evt := range st.drain() {
			emit(conv.stamp(evt))
			if terminal {
				return
			}
		}
	}
	cancelled := ctx.Done()
	for {
		if terminal {
			go func() {
				for range in {
				}
			}()
			break
		}
		select {
		case evt, ok := <-in:
			if !ok {
				goto drained
			}
			flush()
			if terminal {
				continue
			}
			rt.observeLimit(st, evt)
			if ctx.Err() != nil {
				conv.closeStatus = "cancelled"
			}
			for _, next := range conv.convert(evt) {
				emit(next)
				if terminal {
					break
				}
			}
		case <-cancelled:
			// Keep reading until the runner closes the channel. Its
			// completion is the terminal. A dropped completion is mapped
			// once, after the channel closes.
			cancelled = nil
		}
	}
drained:
	if !terminal {
		flush()
	}
	for _, evt := range st.drain() {
		if evt.Terminal() {
			continue
		}
		emit(conv.stamp(evt))
	}
	if !terminal {
		if ctx.Err() != nil {
			for _, next := range conv.closeOpenTools(nil, "cancelled") {
				emit(next)
			}
			emit(conv.stamp(agent.Event{
				RunID:     runID,
				SessionID: req.SessionID,
				Kind:      agent.EventCancelled,
				Time:      time.Now().UTC(),
				Payload: agent.MustPayload(agent.TerminalPayload{
					Status: string(agent.EventCancelled),
					Error:  ctx.Err().Error(),
				}),
			}))
		} else {
			emit(conv.stamp(agent.Event{
				RunID:     runID,
				SessionID: req.SessionID,
				Kind:      agent.EventFailed,
				Time:      time.Now().UTC(),
				Payload: agent.MustPayload(agent.TerminalPayload{
					Status: string(agent.EventFailed),
					Reason: agent.ReasonError,
				}),
			}))
		}
	}
}

func (rt *Runtime) eventHoldTimeout() time.Duration {
	d, err := config.Duration(rt.cfg.Context.EventHoldTimeout)
	if err != nil || d <= 0 {
		return 30 * time.Second
	}
	return d
}

func (rt *Runtime) eventIdleTimeout() time.Duration {
	d, err := config.Duration(rt.cfg.Context.EventIdleTimeout)
	if err != nil || d <= 0 {
		return 5 * time.Minute
	}
	return d
}

func (rt *Runtime) track(h *runHandle) {
	if rt == nil || h == nil {
		return
	}
	rt.mu.Lock()
	if rt.active == nil {
		rt.active = map[*runHandle]struct{}{}
	}
	rt.active[h] = struct{}{}
	rt.mu.Unlock()
}

func (rt *Runtime) untrack(h *runHandle) {
	if rt == nil || h == nil {
		return
	}
	rt.mu.Lock()
	delete(rt.active, h)
	rt.mu.Unlock()
}

func (rt *Runtime) stopPumps() {
	rt.mu.Lock()
	handles := make([]*runHandle, 0, len(rt.active))
	for h := range rt.active {
		handles = append(handles, h)
	}
	rt.mu.Unlock()
	for _, h := range handles {
		_ = h.Close()
	}
}

func (rt *Runtime) toolDrainTimeout() time.Duration {
	d, err := config.Duration(rt.cfg.Context.ToolDrainTimeout)
	if err != nil || d <= 0 {
		return 30 * time.Second
	}
	return d
}

// sessionKey is one app, user, and session. tRPC does not serialize
// concurrent runs, so grape keeps this lock. The entry is removed when
// its refcount hits zero.
type sessionKey struct {
	app     string
	user    string
	session string
}

type countedLock struct {
	mu   sync.Mutex
	refs int
}

// sessionHold is one run's claim on a session lock while its tools drain.
type sessionHold struct {
	runID  string
	unlock func()
	err    error
	st     *runState
}

func (rt *Runtime) sessionOf(user, session string) sessionKey {
	return sessionKey{app: rt.appName, user: user, session: session}
}

func (rt *Runtime) finishSession(user, session, runID string, st *runState, unlock func()) {
	key := rt.sessionOf(user, session)
	if st == nil {
		unlock()
		return
	}
	busy, owner := st.claimFinish()
	if !owner {
		return
	}
	if !busy {
		unlock()
		return
	}
	rt.mu.Lock()
	rt.holds[key] = &sessionHold{
		runID:  runID,
		unlock: unlock,
		st:     st,
		err:    fmt.Errorf("session %s is still finishing tool execution", session),
	}
	rt.mu.Unlock()
	timeout := rt.toolDrainTimeout()
	go func() {
		done := make(chan struct{})
		go func() {
			st.waitTools()
			close(done)
		}()
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-done:
			rt.releaseHold(key, runID)
		case <-timer.C:
			rt.mu.Lock()
			if hold := rt.holds[key]; hold != nil && hold.runID == runID {
				hold.err = fmt.Errorf("session %s is poisoned: a tool did not finish within %s", session, timeout)
			}
			rt.mu.Unlock()
		}
	}()
}

// ForceReleaseSession drops the current poisoned or draining lock.
// It refuses while a tool, streaming reader, or child may still be running
// unless AcceptConcurrentTools is set. That flag is not safe recovery.
// An older run cannot release a later run's lock.
func (rt *Runtime) ForceReleaseSession(sessionID string, rel agent.SessionRelease) error {
	if rt == nil {
		return fmt.Errorf("runtime is nil")
	}
	if rel.UserID == "" || sessionID == "" {
		return agent.Invalid("session", fmt.Errorf("user_id and session_id are required"))
	}
	key := rt.sessionOf(rel.UserID, sessionID)
	rt.mu.Lock()
	hold := rt.holds[key]
	runID := ""
	var st *runState
	if hold != nil {
		runID = hold.runID
		st = hold.st
	}
	rt.mu.Unlock()
	busy := st != nil && st.toolBusy()
	if runID == "" {
		return fmt.Errorf("session %s is not held", sessionID)
	}
	if busy && !rel.AcceptConcurrentTools {
		return agent.Wrap(agent.ErrSession, "session", fmt.Errorf("session %s has a tool still running; ForceRelease is not safe recovery", sessionID))
	}
	if !rt.releaseHold(key, runID) {
		return fmt.Errorf("session %s is not held", sessionID)
	}
	return nil
}

func (rt *Runtime) releaseHold(key sessionKey, runID string) bool {
	rt.mu.Lock()
	hold := rt.holds[key]
	if hold == nil || hold.runID != runID {
		rt.mu.Unlock()
		return false
	}
	delete(rt.holds, key)
	unlock := hold.unlock
	rt.mu.Unlock()
	if unlock != nil {
		unlock()
	}
	return true
}

func (rt *Runtime) lockSession(ctx context.Context, user, session string) (func(), error) {
	key := rt.sessionOf(user, session)
	rt.mu.Lock()
	if rt.closed {
		rt.mu.Unlock()
		return nil, agent.Invalid("start", fmt.Errorf("runtime is closed"))
	}
	if hold := rt.holds[key]; hold != nil && hold.err != nil {
		err := hold.err
		rt.mu.Unlock()
		return nil, agent.Wrap(agent.ErrSession, "session", err)
	}
	lk := rt.locks[key]
	if lk == nil {
		lk = &countedLock{}
		rt.locks[key] = lk
	}
	lk.refs++
	rt.mu.Unlock()
	drop := func() {
		rt.mu.Lock()
		lk.refs--
		if lk.refs <= 0 {
			delete(rt.locks, key)
		}
		rt.mu.Unlock()
	}
	unlock := func() {
		lk.mu.Unlock()
		drop()
	}
	if rt.reject {
		if !lk.mu.TryLock() {
			drop()
			return nil, agent.Wrap(agent.ErrSession, "session", fmt.Errorf("session %s already has an active run", session))
		}
		return unlock, nil
	}
	locked := make(chan struct{})
	go func() {
		lk.mu.Lock()
		close(locked)
	}()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-locked:
			return unlock, nil
		case <-ctx.Done():
			go func() {
				<-locked
				lk.mu.Unlock()
				drop()
			}()
			return nil, agent.Wrap(agent.ErrRunCancelled, "session", ctx.Err())
		case <-ticker.C:
			rt.mu.Lock()
			var err error
			if hold := rt.holds[key]; hold != nil {
				err = hold.err
			}
			rt.mu.Unlock()
			if err != nil {
				go func() {
					<-locked
					lk.mu.Unlock()
					drop()
				}()
				return nil, agent.Wrap(agent.ErrSession, "session", err)
			}
		}
	}
}

// ListSessions returns session metadata for a user.
func (rt *Runtime) ListSessions(ctx context.Context, userID string) ([]agent.SessionInfo, error) {
	list, err := rt.sessions.ListSessions(ctx, session.UserKey{AppName: rt.appName, UserID: userID}, session.WithListSessionOnlyMeta())
	if err != nil {
		return nil, agent.Wrap(agent.ErrSession, "list", err)
	}
	out := make([]agent.SessionInfo, 0, len(list))
	for _, sess := range list {
		if sess == nil {
			continue
		}
		out = append(out, agent.SessionInfo{
			ID:        sess.ID,
			UserID:    sess.UserID,
			AppName:   sess.AppName,
			CreatedAt: sess.CreatedAt,
			UpdatedAt: sess.UpdatedAt,
		})
	}
	return out, nil
}

// DeleteSession removes one session.
func (rt *Runtime) DeleteSession(ctx context.Context, userID, sessionID string) error {
	err := rt.sessions.DeleteSession(ctx, session.Key{
		AppName:   rt.appName,
		UserID:    userID,
		SessionID: sessionID,
	})
	if err != nil {
		return agent.Wrap(agent.ErrSession, "delete", err)
	}
	return nil
}

// MCPStatus reports each configured server after New. Stdio servers that
// were launched have Isolation "none".
func (rt *Runtime) MCPStatus() []mcp.Status {
	if rt == nil {
		return nil
	}
	out := make([]mcp.Status, len(rt.mcpStatus))
	copy(out, rt.mcpStatus)
	return out
}

// Close releases the runner and the SQLite service.
func (rt *Runtime) Close() error {
	if rt == nil {
		return nil
	}
	rt.closeOnce.Do(func() {
		rt.mu.Lock()
		rt.closed = true
		host := rt.taskHost
		rt.mu.Unlock()
		rt.starting.Wait()
		if host != nil {
			host.Close()
		}
		rt.stopPumps()
		if rt.runner != nil {
			rt.closeErr = rt.runner.Close()
		}
		if rt.evolution != nil {
			if err := rt.evolution.Close(); err != nil && rt.closeErr == nil {
				rt.closeErr = err
			}
		}
		if rt.sessions != nil {
			if err := rt.sessions.Close(); err != nil && rt.closeErr == nil {
				rt.closeErr = err
			}
		}
		for _, set := range rt.mcpSets {
			if err := set.Close(); err != nil && rt.closeErr == nil {
				rt.closeErr = err
			}
		}
		if rt.exec != nil {
			if err := rt.exec.closeSkillExecutors(); err != nil && rt.closeErr == nil {
				rt.closeErr = err
			}
		}
	})
	return rt.closeErr
}

type runHandle struct {
	id       string
	events   <-chan agent.Event
	cancel   func() error
	enqueue  func(string) error
	box      *eventBox
	stop     chan struct{}
	stopOnce sync.Once

	mu         sync.Mutex
	result     agent.RunStatus
	resultDone chan struct{}
	userCancel bool
}

func (h *runHandle) noteResult(evt agent.Event, startCanceled bool) {
	if h == nil || !evt.Terminal() {
		return
	}
	var body agent.TerminalPayload
	_ = json.Unmarshal(evt.Payload, &body)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.result.Status != "" {
		return
	}
	delivery := h.result.Delivery
	if h.resultDone != nil {
		defer close(h.resultDone)
	}
	h.result = agent.RunStatus{
		Status:  body.Status,
		Error:   body.Error,
		Reason:  body.Reason,
		Scope:   body.Scope,
		AgentID: body.AgentID,
	}
	if h.result.Status == "" {
		h.result.Status = string(evt.Kind)
	}
	if evt.Kind == agent.EventCancelled && h.result.Cause == "" && (h.userCancel || startCanceled) {
		h.result.Cause = agent.CauseUser
	}
	if delivery == agent.DeliveryAbandoned {
		h.result.Delivery = delivery
		if evt.Kind == agent.EventCancelled && !h.userCancel {
			h.result.Cause = agent.CauseDelivery
		}
	} else {
		h.result.Delivery = agent.DeliveryOpen
	}
}

func (h *runHandle) noteStreamDelivered() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.result.Delivery = agent.DeliveryDelivered
}

func (h *runHandle) noteDeliveryClosed() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.result.Delivery != agent.DeliveryDelivered {
		h.result.Delivery = agent.DeliveryAbandoned
	}
}

func (h *runHandle) Status() agent.RunStatus {
	if h == nil {
		return agent.RunStatus{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.result.Status == "" {
		delivery := h.result.Delivery
		if delivery == "" {
			delivery = agent.DeliveryOpen
		}
		return agent.RunStatus{Status: agent.StatusPending, Delivery: delivery}
	}
	return h.result
}

// Close unblocks the event pump and drops events still buffered.
// A live consumer should read Events until the terminal event. Close is
// for an abandoned handle. The pump also exits after the run finishes
// and the queued events have been delivered.
func (h *runHandle) Close() error {
	if h == nil || h.stop == nil {
		return nil
	}
	h.stopOnce.Do(func() {
		h.mu.Lock()
		pending := h.result.Status == ""
		if h.result.Delivery != agent.DeliveryDelivered {
			h.result.Delivery = agent.DeliveryAbandoned
		}
		h.mu.Unlock()
		if pending && h.cancel != nil {
			_ = h.cancel()
		}
		if h.box != nil {
			h.box.abandon()
		}
		close(h.stop)
	})
	return nil
}

func (h *runHandle) ID() string                 { return h.id }
func (h *runHandle) Events() <-chan agent.Event { return h.events }
func (h *runHandle) Cancel() error {
	if h == nil || h.cancel == nil {
		return nil
	}
	h.mu.Lock()
	h.userCancel = true
	h.mu.Unlock()
	return h.cancel()
}
func (h *runHandle) Enqueue(message string) error {
	if h.enqueue == nil {
		return agent.Wrap(agent.ErrInvalidConfig, "enqueue", fmt.Errorf("runner cannot steer"))
	}
	return h.enqueue(message)
}

// Compile-time check that the concrete handle satisfies the public interface.
var _ agent.RunHandle = (*runHandle)(nil)
var _ agent.Runtime = (*Runtime)(nil)
