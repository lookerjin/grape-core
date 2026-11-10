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
	mu       sync.Mutex
	wake     chan struct{}
	finished chan struct{}
	items    []agent.Event
	closed   bool
	max      int
	dropped  int
}

func newEventBox(max int) *eventBox {
	if max < 2 {
		max = 2
	}
	return &eventBox{max: max, wake: make(chan struct{}, 1), finished: make(chan struct{})}
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

func (b *eventBox) takeDroppedLocked() (agent.Event, bool) {
	if b.dropped == 0 {
		return agent.Event{}, false
	}
	n := b.dropped
	b.dropped = 0
	return droppedMarker(n), true
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
	if !b.closed {
		b.closed = true
		close(b.finished)
	}
	b.mu.Unlock()
	b.signal()
}

// abandon drops buffered events and refuses further pushes. Close uses
// this so a finished handle does not keep filling the queue.
func (b *eventBox) abandon() {
	b.mu.Lock()
	if !b.closed {
		b.closed = true
		close(b.finished)
	}
	b.items = nil
	b.dropped = 0
	b.mu.Unlock()
	b.signal()
}

// pump delivers in order. The output buffer can absorb a short completed run
// without a reader. Once execution finishes, a blocked send has a deadline.
func (b *eventBox) pump(out chan<- agent.Event, stop <-chan struct{}, hold, idle time.Duration) bool {
	defer b.abandon()
	finished := b.finished
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	sent := 0
	for {
		evt, ok := b.pop()
		if !ok {
			select {
			case <-b.wake:
				continue
			case <-b.finished:
				// close follows the final push. Recheck the queue after observing it.
				evt, ok = b.pop()
				if !ok {
					return false
				}
			case <-stop:
				return false
			}
		}
		var timeout <-chan time.Time
		for {
			if finished == nil {
				wait := hold
				if sent > len(out) {
					wait = idle
				}
				if timer == nil {
					timer = time.NewTimer(wait)
				} else {
					timer.Reset(wait)
				}
				timeout = timer.C
			}
			select {
			case out <- evt:
				sent++
			case <-stop:
				return false
			case <-finished:
				finished = nil
				continue
			case <-timeout:
				return false
			}
			break
		}
		if evt.Terminal() {
			return true
		}
	}
}

func (b *eventBox) pop() (agent.Event, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.items) > 0 || b.closed {
		if marker, ok := b.takeDroppedLocked(); ok {
			return marker, true
		}
	}
	if len(b.items) == 0 {
		return agent.Event{}, false
	}
	evt := b.items[0]
	b.items[0] = agent.Event{}
	b.items = b.items[1:]
	return evt, true
}
