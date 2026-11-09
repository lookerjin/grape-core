package trpcruntime

import (
	"fmt"
	"sync"
	"time"

	"github.com/lookerjin/grape-core/agent"
)

// eventBox queues run events for a consumer.
//
// Text deltas are the only events that are dropped, and only when the
// consumer is behind the soft cap. Tool, permission, error, hook, and
// other events are kept and the queue grows to hold them. A terminal
// event is always kept. Each burst of dropped deltas is reported with
// one "dropped N events" marker.
type eventBox struct {
	mu      sync.Mutex
	wake    chan struct{}
	items   []agent.Event
	closed  bool
	max     int
	dropped int
}

func newEventBox(max int) *eventBox {
	if max < 2 {
		max = 2
	}
	return &eventBox{max: max, wake: make(chan struct{}, 1)}
}

func (b *eventBox) signal() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

func (b *eventBox) push(evt agent.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	if evt.Kind == agent.EventTextDelta {
		for len(b.items) >= b.max-1 && b.dropOldestDelta() {
		}
		if len(b.items) >= b.max-1 {
			b.dropped++
			return
		}
	}
	b.flushDroppedLocked()
	b.items = append(b.items, evt)
	b.signal()
}

func (b *eventBox) dropOldestDelta() bool {
	for i, item := range b.items {
		if item.Kind == agent.EventTextDelta {
			b.items = append(b.items[:i], b.items[i+1:]...)
			b.dropped++
			return true
		}
	}
	return false
}

func (b *eventBox) flushDroppedLocked() {
	if b.dropped == 0 {
		return
	}
	b.items = append(b.items, droppedMarker(b.dropped))
	b.dropped = 0
}

func droppedMarker(n int) agent.Event {
	return agent.Event{
		Kind: agent.EventRaw,
		Time: time.Now().UTC(),
		Payload: agent.MustPayload(map[string]any{
			"dropped": n,
			"message": fmt.Sprintf("dropped %d events", n),
		}),
	}
}

func (b *eventBox) close() {
	b.mu.Lock()
	b.flushDroppedLocked()
	b.closed = true
	b.mu.Unlock()
	b.signal()
}

// pump sends queued events until the box is drained or stop is closed.
// Closing stop drops whatever is still buffered so an abandoned handle
// does not leave this goroutine running.
func (b *eventBox) pump(out chan<- agent.Event, stop <-chan struct{}) {
	defer close(out)
	for {
		evt, ok := b.pop(stop)
		if !ok {
			return
		}
		select {
		case out <- evt:
		case <-stop:
			return
		}
	}
}

func (b *eventBox) pop(stop <-chan struct{}) (agent.Event, bool) {
	for {
		b.mu.Lock()
		if len(b.items) > 0 {
			evt := b.items[0]
			b.items = b.items[1:]
			b.mu.Unlock()
			return evt, true
		}
		if b.closed {
			b.mu.Unlock()
			return agent.Event{}, false
		}
		b.mu.Unlock()
		select {
		case <-b.wake:
		case <-stop:
			return agent.Event{}, false
		}
	}
}
