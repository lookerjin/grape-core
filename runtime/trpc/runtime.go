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
	"github.com/lookerjin/grape-core/internal/journal"
	"github.com/lookerjin/grape-core/internal/paths"
	trpcagent "trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/agent/llmagent"
	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/runner"
	"trpc.group/trpc-go/trpc-agent-go/session"
	sqlitesession "trpc.group/trpc-go/trpc-agent-go/session/sqlite"
	"trpc.group/trpc-go/trpc-agent-go/tool"

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
	tools       []tool.Tool
	decider     agent.Decider
	reject      bool
	journal     *journal.Writer
	secrets     []string
	maxField    int
	instruction string

	mu    sync.Mutex
	locks map[string]*sync.Mutex

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
		instruction = "You are grape-core, a local assistant. Use tools only when they are provided. Never claim that a command ran unless a tool result says so."
	}
	agentOpts := []llmagent.Option{
		llmagent.WithModel(mdl),
		llmagent.WithInstruction(instruction),
		llmagent.WithGenerationConfig(generationConfig(cfg)),
		llmagent.WithEnableCodeExecutionResponseProcessor(false),
		llmagent.WithMaxLLMCalls(positiveLimit(cfg.Context.MaxLLMCalls)),
		llmagent.WithMaxToolIterations(positiveLimit(cfg.Context.MaxToolIterations)),
	}
	if len(opts.Tools) > 0 {
		agentOpts = append(agentOpts, llmagent.WithTools(opts.Tools))
	}
	ag := llmagent.New("grape", agentOpts...)
	appName := opts.AppName
	if appName == "" {
		appName = defaultAppName
	}
	r := runner.NewRunner(appName, ag, runner.WithSessionService(sessions))
	secrets := secretValues(cfg)
	var jw *journal.Writer
	if cfg.Journal.Enabled {
		jw, err = journal.Open(cfg.Journal.Path, cfg.Journal.MaxFieldBytes, secrets)
		if err != nil {
			_ = r.Close()
			_ = sessions.Close()
			return nil, agent.Wrap(agent.ErrInvalidConfig, "journal", err)
		}
	}
	return &Runtime{
		cfg:         cfg,
		appName:     appName,
		runner:      r,
		sessions:    sessions,
		counter:     counter,
		tools:       append([]tool.Tool(nil), opts.Tools...),
		decider:     opts.Decider,
		reject:      opts.RejectConcurrentSession,
		journal:     jw,
		secrets:     secrets,
		maxField:    cfg.Journal.MaxFieldBytes,
		instruction: instruction,
		locks:       map[string]*sync.Mutex{},
	}, nil
}

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
	unlock, err := rt.lockSession(req.SessionID)
	if err != nil {
		return nil, err
	}
	runID := uuid.NewString()
	runCtx, cancel := context.WithCancel(ctx)
	events, err := rt.runner.Run(
		runCtx,
		req.UserID,
		req.SessionID,
		model.NewUserMessage(req.Message),
		trpcagent.WithRequestID(runID),
	)
	if err != nil {
		cancel()
		unlock()
		return nil, classifyRunError(err)
	}
	ch := make(chan agent.Event, 256)
	h := &runHandle{
		id:     runID,
		events: ch,
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
	go rt.forward(runCtx, req, runID, events, ch, unlock)
	return h, nil
}

func classifyRunError(err error) error {
	if err == nil {
		return nil
	}
	return agent.Wrap(agent.ErrModel, "run", err)
}

func (rt *Runtime) forward(ctx context.Context, req agent.RunRequest, runID string, in <-chan *event.Event, out chan<- agent.Event, unlock func()) {
	defer unlock()
	defer close(out)
	conv := newConverter(runID, req.SessionID, rt.maxField, rt.secrets)
	terminal := false
	emit := func(evt agent.Event) {
		if evt.RunID == "" {
			evt.RunID = runID
		}
		if evt.SessionID == "" {
			evt.SessionID = req.SessionID
		}
		if rt.journal != nil {
			_ = rt.journal.Append(evt)
		}
		out <- evt
		if evt.Terminal() {
			terminal = true
		}
	}
	for evt := range in {
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

func (rt *Runtime) lockSession(id string) (func(), error) {
	rt.mu.Lock()
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
	} else {
		mu.Lock()
	}
	return mu.Unlock, nil
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

// Close releases the runner and the SQLite service.
func (rt *Runtime) Close() error {
	if rt == nil {
		return nil
	}
	rt.closeOnce.Do(func() {
		if rt.runner != nil {
			rt.closeErr = rt.runner.Close()
		}
		if rt.sessions != nil {
			if err := rt.sessions.Close(); err != nil && rt.closeErr == nil {
				rt.closeErr = err
			}
		}
	})
	return rt.closeErr
}

type runHandle struct {
	id      string
	events  <-chan agent.Event
	cancel  func() error
	enqueue func(string) error
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
