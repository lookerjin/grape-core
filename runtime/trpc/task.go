package trpcruntime

import (
	"context"
	"encoding/json"
	"errors"
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

	mu        sync.Mutex
	cancel    map[string]context.CancelFunc
	closed    bool
	wg        sync.WaitGroup
	closeOnce sync.Once
}

// taskGoroutineWait is how long Runtime.Close waits for hosted task
// goroutines after cancelling them. It is not a caller-facing setting.
const taskGoroutineWait = 5 * time.Second

// NewTaskHost stores task status next to the runtime data directory.
func NewTaskHost(rt *Runtime) (*TaskHost, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.closed {
		return nil, errors.New("runtime closed")
	}
	if rt.taskHost != nil {
		return rt.taskHost, nil
	}
	index := filepath.Join(filepath.Dir(rt.cfg.Session.Path), "tasks.json")
	if err := os.MkdirAll(filepath.Dir(index), 0o755); err != nil {
		return nil, err
	}
	h := &TaskHost{rt: rt, index: index, cancel: map[string]context.CancelFunc{}}
	if err := h.normalize(); err != nil {
		return nil, err
	}
	rt.taskHost = h
	return h, nil
}

// normalize marks tasks left running by a previous process as failed.
// Terminal rows stay. There is no retention limit.
func (h *TaskHost) normalize() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	tasks, err := h.loadLocked()
	if err != nil {
		return err
	}
	changed := false
	now := time.Now().UTC()
	for id, task := range tasks {
		switch task.Status {
		case "completed", "failed", "cancelled":
			continue
		default:
			task.Status = "failed"
			task.Error = agent.TaskInterrupted
			task.UpdatedAt = now
			tasks[id] = task
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return h.writeLocked(tasks)
}

// Close cancels hosted tasks, waits a bounded time for their goroutines,
// and drops this host from the runtime.
func (h *TaskHost) Close() {
	if h == nil {
		return
	}
	h.closeOnce.Do(func() {
		h.cancelAll()
		done := make(chan struct{})
		go func() {
			h.wg.Wait()
			h.rt.mu.Lock()
			h.rt.taskHost = nil
			h.rt.mu.Unlock()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(taskGoroutineWait):
		}
	})
}

func (h *TaskHost) cancelAll() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.closed = true
	cancels := make([]context.CancelFunc, 0, len(h.cancel))
	for _, cancel := range h.cancel {
		cancels = append(cancels, cancel)
	}
	h.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (h *TaskHost) clearCancel(id string) {
	h.mu.Lock()
	if cancel := h.cancel[id]; cancel != nil {
		cancel()
	}
	delete(h.cancel, id)
	h.mu.Unlock()
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
	if h.closed {
		h.mu.Unlock()
		cancel()
		return agent.Task{}, errors.New("task host closed")
	}
	h.wg.Add(1)
	h.cancel[task.ID] = cancel
	h.mu.Unlock()
	if err := h.save(task); err != nil {
		h.clearCancel(task.ID)
		h.wg.Done()
		cancel()
		return agent.Task{}, err
	}
	go func() {
		defer h.wg.Done()
		h.run(ctx, task, req)
	}()
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
	defer h.clearCancel(task.ID)
	handle, err := h.rt.Start(ctx, req)
	if err != nil {
		task.Status = "failed"
		task.Error = err.Error()
		if ctx.Err() != nil {
			task.Status = "cancelled"
		}
		task.UpdatedAt = time.Now().UTC()
		_ = h.save(task)
		return
	}
	task.RunID = handle.ID()
	_ = h.save(task)
	for range handle.Events() {
	}
	// The stream can close without the terminal when the runtime is closed.
	// Status is the result after the run stops.
	<-handle.(*runHandle).resultDone
	status := handle.Status()
	switch status.Status {
	case string(agent.EventCancelled):
		task.Status = "cancelled"
	case string(agent.EventFailed):
		task.Status = "failed"
		if task.Error == "" {
			task.Error = status.Error
		}
	case string(agent.EventCompleted):
		task.Status = "completed"
	default:
		if ctx.Err() != nil {
			task.Status = "cancelled"
		} else {
			task.Status = "failed"
		}
	}
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
	return h.writeLocked(tasks)
}

func (h *TaskHost) writeLocked(tasks map[string]agent.Task) error {
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
