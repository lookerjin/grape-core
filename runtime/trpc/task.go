package trpcruntime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/lookerjin/grape-core/agent"
)

// TaskHost runs work on a context it owns. Runtime.Start still uses the
// caller context. Cancelling some other context does not stop a hosted task.
type TaskHost struct {
	rt    *Runtime
	index string

	mu     sync.Mutex
	cancel map[string]context.CancelFunc
}

// NewTaskHost stores task status next to the runtime data directory.
func NewTaskHost(rt *Runtime) (*TaskHost, error) {
	index := filepath.Join(filepath.Dir(rt.cfg.Session.Path), "tasks.json")
	if err := os.MkdirAll(filepath.Dir(index), 0o755); err != nil {
		return nil, err
	}
	return &TaskHost{rt: rt, index: index, cancel: map[string]context.CancelFunc{}}, nil
}

// Start launches req on a host-owned context and returns immediately.
func (h *TaskHost) Start(req agent.RunRequest) (agent.Task, error) {
	ctx, cancel := context.WithCancel(context.Background())
	task := agent.Task{
		ID:        uuid.NewString(),
		SessionID: req.SessionID,
		UserID:    req.UserID,
		Status:    "running",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	h.mu.Lock()
	h.cancel[task.ID] = cancel
	h.mu.Unlock()
	if err := h.save(task); err != nil {
		cancel()
		return agent.Task{}, err
	}
	go h.run(ctx, task, req)
	return task, nil
}

// Cancel stops a hosted task.
func (h *TaskHost) Cancel(id string) {
	h.mu.Lock()
	cancel := h.cancel[id]
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Get returns the last stored task record.
func (h *TaskHost) Get(id string) (agent.Task, bool) {
	tasks, err := h.load()
	if err != nil {
		return agent.Task{}, false
	}
	task, ok := tasks[id]
	return task, ok
}

func (h *TaskHost) run(ctx context.Context, task agent.Task, req agent.RunRequest) {
	handle, err := h.rt.Start(ctx, req)
	if err != nil {
		task.Status = "failed"
		task.Error = err.Error()
		task.UpdatedAt = time.Now().UTC()
		_ = h.save(task)
		return
	}
	task.RunID = handle.ID()
	_ = h.save(task)
	status := "completed"
	for evt := range handle.Events() {
		if evt.Kind == agent.EventFailed {
			status = "failed"
		}
		if evt.Kind == agent.EventCancelled {
			status = "cancelled"
		}
	}
	task.Status = status
	task.UpdatedAt = time.Now().UTC()
	_ = h.save(task)
}

func (h *TaskHost) save(task agent.Task) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	tasks, err := h.loadLocked()
	if err != nil {
		return err
	}
	tasks[task.ID] = task
	raw, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return err
	}
	tmp := h.index + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, h.index)
}

func (h *TaskHost) load() (map[string]agent.Task, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.loadLocked()
}

func (h *TaskHost) loadLocked() (map[string]agent.Task, error) {
	raw, err := os.ReadFile(h.index)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]agent.Task{}, nil
		}
		return nil, err
	}
	var tasks map[string]agent.Task
	if err := json.Unmarshal(raw, &tasks); err != nil {
		return nil, err
	}
	if tasks == nil {
		tasks = map[string]agent.Task{}
	}
	return tasks, nil
}
