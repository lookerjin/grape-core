package trpcruntime

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
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
	"trpc.group/trpc-go/trpc-agent-go/runner"
	"trpc.group/trpc-go/trpc-agent-go/session"
	sqlitesession "trpc.group/trpc-go/trpc-agent-go/session/sqlite"
	"trpc.group/trpc-go/trpc-agent-go/tool"
	agenttool "trpc.group/trpc-go/trpc-agent-go/tool/agent"
	"trpc.group/trpc-go/trpc-agent-go/tool/function"

	_ "github.com/mattn/go-sqlite3"
)

const defaultAppName = "grape-core"

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
	FixedAgent    trpcagent.Agent
	SubAgents     []trpcagent.Agent
	// ToolSpecs classifies extra tools. An empty Kind is side_effect.
	// Trusted tools skip the approval gate; Options.Tools are untrusted.
	ToolSpecs         []ToolSpec
	EvolutionReviewer evolution.Reviewer
}

// Runtime is a tRPC-backed agent.Runtime.
//
// It owns the SQLite session service it opens. Close releases that database
// and the runner. A caller-supplied model is not closed.
type Runtime struct {
	cfg         config.Config
	appName     string
	runner      runner.Runner
	sessions    *sqlitesession.Service
	counter     *countingModel
	model       model.Model
	exec        *commandExecutor
	tools       []tool.Tool
	specs       []ToolSpec
	skillRoots  []string
	ruleText    string
	ruleSources []rules.Source
	globalRules string
	hookRunner  *hooks.Runner
	mcpServers  []mcp.Server
	resolved    plugin.Resolved
	fixed       trpcagent.Agent
	subs        []trpcagent.Agent
	evolution   evolution.Service
	mcpSets     []tool.ToolSet
	mcpStatus   []mcp.Status
	decider     agent.Decider
	reject      bool
	journal     *journal.Writer
	secrets     []string
	maxField    int
	instruction string

	mu    sync.Mutex
	locks map[string]*sync.Mutex
	// holds is the in-flight drain for a session, keyed so an older run
	// cannot release or poison the run that replaced it.
	holds map[string]*sessionHold

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
		cfg:         cfg,
		appName:     appName,
		sessions:    sessions,
		counter:     counter,
		model:       mdl,
		exec:        newCommandExecutor(cfg, filepath.Join(os.TempDir(), "grape-core-sandbox"), dataDir, opts.Platform, opts.External),
		tools:       append([]tool.Tool(nil), opts.Tools...),
		specs:       append([]ToolSpec(nil), opts.ToolSpecs...),
		decider:     opts.Decider,
		reject:      opts.RejectConcurrentSession,
		secrets:     secretValues(cfg),
		maxField:    cfg.Journal.MaxFieldBytes,
		instruction: instruction,
		locks:       map[string]*sync.Mutex{},
		holds:       map[string]*sessionHold{},
		fixed:       opts.FixedAgent,
		subs:        append([]trpcagent.Agent(nil), opts.SubAgents...),
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
		runner.WithPlugins(&hookPlugin{rt: rt}),
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
	tools, err := rt.workspaceTools(ws, allowWrite)
	if err != nil {
		return nil, err
	}
	for _, spec := range rt.specs {
		if spec.Tool == nil {
			continue
		}
		if spec.Trusted {
			tools = append(tools, spec.Tool)
			continue
		}
		kind := spec.Kind
		if kind == "" {
			kind = "side_effect"
		}
		tools = append(tools, rt.gate(spec.Tool, kind))
	}
	for _, item := range rt.tools {
		tools = append(tools, rt.gate(item, "side_effect"))
	}
	mode := ""
	if ro.RuntimeState != nil {
		mode, _ = ro.RuntimeState["mode"].(string)
	}
	if len(rt.subs) > 0 {
		for _, sub := range rt.subs {
			tools = append(tools, rt.gate(agenttool.NewTool(sub), "side_effect"))
		}
	}
	if rt.cfg.Memory.Enabled {
		tools = append(tools, rt.memoryTool())
	}
	instruction := rt.instruction
	if text, err := rt.ruleTextFor(ws); err == nil && text != "" {
		instruction += "\n\nRules:\n" + text
	}
	opts := []llmagent.Option{
		llmagent.WithModel(rt.model),
		llmagent.WithInstruction(instruction),
		llmagent.WithGenerationConfig(generationConfig(rt.cfg)),
		llmagent.WithEnableCodeExecutionResponseProcessor(false),
		llmagent.WithWorkspaceExecSurfaceEnabled(false),
		llmagent.WithMaxLLMCalls(positiveLimit(rt.cfg.Context.MaxLLMCalls)),
		llmagent.WithMaxToolIterations(positiveLimit(rt.cfg.Context.MaxToolIterations)),
		llmagent.WithTools(trackTools(tools)),
	}
	if len(rt.mcpSets) > 0 {
		opts = append(opts, llmagent.WithToolSets(rt.mcpSets))
	}
	if repo, err := rt.skillRepository(); err != nil {
		return nil, err
	} else if repo != nil {
		opts = append(opts,
			llmagent.WithSkills(repo),
			llmagent.WithSkillToolProfile(llmagent.SkillToolProfileKnowledgeOnly),
		)
	}
	if mode == string(agent.ModeGoal) {
		retries := rt.cfg.Goal.MaxIterations
		if retries <= 0 {
			retries = 1
		}
		opts = append(opts, llmagent.WithExtensions(goal.New(goal.WithMaxRetries(retries))))
	}
	// Subagents are exposed only as gated agent tools. WithSubAgents would
	// also register transfer_to_agent, which bypasses that gate.
	return llmagent.New("grape", opts...), nil
}

func (rt *Runtime) memoryTool() tool.Tool {
	return function.NewFunctionTool(
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
	)
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
	unlock, err := rt.lockSession(ctx, req.SessionID)
	if err != nil {
		return nil, err
	}
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
	events, err := rt.runner.Run(
		runCtx,
		req.UserID,
		req.SessionID,
		model.NewUserMessage(req.Message),
		trpcagent.WithRequestID(runID),
		trpcagent.WithRuntimeState(map[string]any{
			"workspace": workspace,
			"mode":      string(req.Mode),
		}),
	)
	if err != nil {
		cancel()
		unlock()
		return nil, classifyRunError(err)
	}
	box := newEventBox(256)
	ch := make(chan agent.Event)
	h := &runHandle{
		id:     runID,
		events: ch,
		box:    box,
		stop:   make(chan struct{}),
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
	go box.pump(ch, h.stop)
	go rt.forward(runCtx, req, runID, st, events, box, unlock)
	return h, nil
}

func classifyRunError(err error) error {
	if err == nil {
		return nil
	}
	return agent.Wrap(agent.ErrModel, "run", err)
}

func (rt *Runtime) forward(ctx context.Context, req agent.RunRequest, runID string, st *runState, in <-chan *event.Event, out *eventBox, unlock func()) {
	defer rt.finishSession(req.SessionID, runID, st, unlock)
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
		if ctx.Err() != nil && !evt.Terminal() {
			return
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
			if ctx.Err() != nil {
				continue
			}
			flush()
			if terminal {
				continue
			}
			for _, next := range conv.convert(evt) {
				if next.Kind == agent.EventCompleted && ctx.Err() != nil {
					next.Kind = agent.EventCancelled
					next.Payload = agent.MustPayload(agent.TerminalPayload{
						Status: string(agent.EventCancelled),
						Error:  ctx.Err().Error(),
					})
				}
				emit(next)
				if terminal {
					break
				}
			}
		case <-cancelled:
			cancelled = nil
			if !terminal {
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
			}
			go func() {
				for range in {
				}
			}()
			return
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
		status := agent.EventFailed
		msg := "run ended without a terminal event"
		if ctx.Err() != nil {
			status = agent.EventCancelled
			msg = ctx.Err().Error()
		}
		emit(conv.stamp(agent.Event{
			RunID:     runID,
			SessionID: req.SessionID,
			Kind:      status,
			Time:      time.Now().UTC(),
			Payload: agent.MustPayload(agent.TerminalPayload{
				Status: string(status),
				Error:  msg,
			}),
		}))
	}
}

func (rt *Runtime) toolDrainTimeout() time.Duration {
	d, err := config.Duration(rt.cfg.Context.ToolDrainTimeout)
	if err != nil || d <= 0 {
		return 30 * time.Second
	}
	return d
}

// sessionHold is one run's claim on a session lock while its tools drain.
type sessionHold struct {
	runID  string
	unlock func()
	err    error
}

func (rt *Runtime) finishSession(id, runID string, st *runState, unlock func()) {
	if st == nil || !st.captureBusy() {
		unlock()
		return
	}
	rt.mu.Lock()
	rt.holds[id] = &sessionHold{
		runID:  runID,
		unlock: unlock,
		err:    fmt.Errorf("session %s is still finishing tool execution", id),
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
			rt.releaseHold(id, runID)
		case <-timer.C:
			rt.mu.Lock()
			if hold := rt.holds[id]; hold != nil && hold.runID == runID {
				hold.err = fmt.Errorf("session %s is poisoned: a tool did not finish within %s", id, timeout)
			}
			rt.mu.Unlock()
		}
	}()
}

// ForceReleaseSession drops the current poisoned or draining lock.
// The stuck tool may still be running. The next Start is allowed.
// An older run cannot release a later run's lock.
func (rt *Runtime) ForceReleaseSession(id string) error {
	if rt == nil {
		return fmt.Errorf("runtime is nil")
	}
	rt.mu.Lock()
	hold := rt.holds[id]
	runID := ""
	if hold != nil {
		runID = hold.runID
	}
	rt.mu.Unlock()
	if runID == "" || !rt.releaseHold(id, runID) {
		return fmt.Errorf("session %s is not held", id)
	}
	return nil
}

func (rt *Runtime) releaseHold(id, runID string) bool {
	rt.mu.Lock()
	hold := rt.holds[id]
	if hold == nil || hold.runID != runID {
		rt.mu.Unlock()
		return false
	}
	delete(rt.holds, id)
	unlock := hold.unlock
	rt.mu.Unlock()
	if unlock != nil {
		unlock()
	}
	return true
}

func (rt *Runtime) lockSession(ctx context.Context, id string) (func(), error) {
	rt.mu.Lock()
	if hold := rt.holds[id]; hold != nil && hold.err != nil {
		err := hold.err
		rt.mu.Unlock()
		return nil, agent.Wrap(agent.ErrSession, "session", err)
	}
	mu := rt.locks[id]
	if mu == nil {
		mu = &sync.Mutex{}
		rt.locks[id] = mu
	}
	rt.mu.Unlock()
	if rt.reject {
		if !mu.TryLock() {
			return nil, agent.Wrap(agent.ErrSession, "session", fmt.Errorf("session %s already has an active run", id))
		}
		return mu.Unlock, nil
	}
	locked := make(chan struct{})
	go func() {
		mu.Lock()
		close(locked)
	}()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-locked:
			return mu.Unlock, nil
		case <-ctx.Done():
			go func() {
				<-locked
				mu.Unlock()
			}()
			return nil, agent.Wrap(agent.ErrRunCancelled, "session", ctx.Err())
		case <-ticker.C:
			rt.mu.Lock()
			var err error
			if hold := rt.holds[id]; hold != nil {
				err = hold.err
			}
			rt.mu.Unlock()
			if err != nil {
				go func() {
					<-locked
					mu.Unlock()
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
		if h.box != nil {
			h.box.abandon()
		}
		close(h.stop)
	})
	return nil
}

func (h *runHandle) ID() string                 { return h.id }
func (h *runHandle) Events() <-chan agent.Event { return h.events }
func (h *runHandle) Cancel() error              { return h.cancel() }
func (h *runHandle) Enqueue(message string) error {
	if h.enqueue == nil {
		return agent.Wrap(agent.ErrInvalidConfig, "enqueue", fmt.Errorf("runner cannot steer"))
	}
	return h.enqueue(message)
}

// Compile-time check that the concrete handle satisfies the public interface.
var _ agent.RunHandle = (*runHandle)(nil)
var _ agent.Runtime = (*Runtime)(nil)
