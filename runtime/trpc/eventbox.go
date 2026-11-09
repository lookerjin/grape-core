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
	cond    *sync.Cond
	items   []agent.Event
	closed  bool
	max     int
	dropped int
}

func newEventBox(max int) *eventBox {
	if max < 2 {
		max = 2
	}
	b := &eventBox{max: max}
	b.cond = sync.NewCond(&b.mu)
	return b
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
	b.cond.Signal()
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
	b.cond.Broadcast()
	b.mu.Unlock()
}

func (b *eventBox) pump(out chan<- agent.Event) {
	defer close(out)
	for {
		b.mu.Lock()
		for len(b.items) == 0 && !b.closed {
			b.cond.Wait()
		}
		if len(b.items) == 0 {
			b.mu.Unlock()
			return
		}
		evt := b.items[0]
		b.items = b.items[1:]
		b.mu.Unlock()
		out <- evt
	}
}
