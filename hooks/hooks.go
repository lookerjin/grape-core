package hooks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lookerjin/grape-core/sandbox"
)

// Supported events have a tRPC callback or a documented shell position.
// OnError has no dedicated callback and is rejected.
const (
	BeforeRun   = "BeforeRun"
	AfterRun    = "AfterRun"
	BeforeModel = "BeforeModel"
	AfterModel  = "AfterModel"
	BeforeTool  = "BeforeTool"
	AfterTool   = "AfterTool"
	OnEvent     = "OnEvent"
	OnError     = "OnError"
)

// Spec is one shell hook.
type Spec struct {
	Event      string        `json:"event"`
	Command    string        `json:"command"`
	Args       []string      `json:"args,omitempty"`
	Timeout    time.Duration `json:"timeout,omitempty"`
	Blocking   bool          `json:"blocking"`
	ExpectJSON bool          `json:"expect_json,omitempty"`
}

// Result is the executor result plus whether the hook blocked the step.
type Result struct {
	Event    string
	ExitCode int
	Stdout   string
	Stderr   string
	Blocked  bool
}

// File is the on-disk hooks.json document.
type File struct {
	Hooks []Spec `json:"hooks"`
}

// Load reads hooks.json.
func Load(path string) ([]Spec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// Parse decodes a hooks document and checks event names.
func Parse(raw []byte) ([]Spec, error) {
	var doc File
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	for i := range doc.Hooks {
		if doc.Hooks[i].Timeout == 0 {
			doc.Hooks[i].Timeout = 30 * time.Second
		}
		if !Supported(doc.Hooks[i].Event) {
			return nil, fmt.Errorf("hook event %q is unsupported", doc.Hooks[i].Event)
		}
		if strings.TrimSpace(doc.Hooks[i].Command) == "" {
			return nil, fmt.Errorf("hook command is empty")
		}
	}
	return doc.Hooks, nil
}

// Supported reports whether event has a real callback position.
func Supported(event string) bool {
	switch event {
	case BeforeRun, AfterRun, BeforeTool, AfterTool, BeforeModel, AfterModel, OnEvent:
		return true
	default:
		return false
	}
}

// Runner executes shell hooks through Executor.
type Runner struct {
	exec  sandbox.Executor
	hooks []Spec
	limit chan struct{}
	seen  map[string]struct{}
}

// NewRunner bounds concurrent hook execution.
func NewRunner(exec sandbox.Executor, hooks []Spec, maxConcurrent int) *Runner {
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	return &Runner{
		exec:  exec,
		hooks: append([]Spec(nil), hooks...),
		limit: make(chan struct{}, maxConcurrent),
		seen:  map[string]struct{}{},
	}
}

// Run executes hooks for event. input is the JSON payload.
// A blocking hook with a non-zero exit, timeout, or executor error stops
// the step. Duplicate event+command+input pairs in one runner are skipped.
func (r *Runner) Run(ctx context.Context, event, workspace, input string) ([]Result, error) {
	if !Supported(event) {
		return nil, fmt.Errorf("hook event %q is unsupported", event)
	}
	var out []Result
	if strings.TrimSpace(input) != "" && !json.Valid([]byte(input)) {
		return nil, fmt.Errorf("hook input is not JSON")
	}
	for _, hook := range r.hooks {
		if hook.Event != event {
			continue
		}
		key := dedupeKey(hook, input)
		if _, ok := r.seen[key]; ok {
			continue
		}
		r.seen[key] = struct{}{}
		select {
		case r.limit <- struct{}{}:
		case <-ctx.Done():
			return out, ctx.Err()
		}
		result, err := r.one(ctx, hook, workspace, input)
		<-r.limit
		out = append(out, result)
		if err != nil && hook.Blocking {
			result.Blocked = true
			if len(out) > 0 {
				out[len(out)-1].Blocked = true
			}
			return out, err
		}
	}
	return out, nil
}

func (r *Runner) one(ctx context.Context, hook Spec, workspace, input string) (Result, error) {
	timeout := hook.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	argv := append([]string{hook.Command}, hook.Args...)
	res, err := r.exec.Execute(runCtx, sandbox.Request{
		Workspace: workspace,
		Argv:      argv,
		Timeout:   timeout,
		Env:       map[string]string{"GRAPE_HOOK_INPUT": input},
	})
	result := Result{Event: hook.Event, ExitCode: res.ExitCode, Stdout: res.Stdout, Stderr: res.Stderr}
	if err != nil {
		if hook.Blocking {
			result.Blocked = true
		}
		return result, err
	}
	if res.ExitCode != 0 {
		err = fmt.Errorf("hook %s exited %d", hook.Event, res.ExitCode)
	} else if hook.ExpectJSON && !json.Valid([]byte(strings.TrimSpace(res.Stdout))) {
		err = fmt.Errorf("hook %s returned malformed JSON", hook.Event)
	}
	if err != nil && hook.Blocking {
		result.Blocked = true
		return result, err
	}
	return result, nil
}

func dedupeKey(hook Spec, input string) string {
	sum := sha256.Sum256([]byte(hook.Event + "\x00" + hook.Command + "\x00" + strings.Join(hook.Args, "\x00") + "\x00" + input))
	return hex.EncodeToString(sum[:])
}
