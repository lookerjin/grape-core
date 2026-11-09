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
	b.closed = true
	b.mu.Unlock()
	b.signal()
}

// abandon drops buffered events and refuses further pushes. Close uses
// this so a finished handle does not keep filling the queue.
func (b *eventBox) abandon() {
	b.mu.Lock()
	b.closed = true
	b.items = nil
	b.dropped = 0
	b.mu.Unlock()
	b.signal()
}

// pump sends queued events until the box is drained or stop is closed.
// After the run is finished, idle is how long the pump waits for a
// consumer who has not taken an event. The first take cancels that
// clock. A consumer who has started reading keeps every tool, permission,
// and error event, even when the gap between takes is longer than idle.
// Close and Runtime.Close still release the pump.
func (b *eventBox) pump(out chan<- agent.Event, stop <-chan struct{}, idle time.Duration) {
	defer close(out)
	if idle <= 0 {
		idle = 30 * time.Second
	}
	var timer *time.Timer
	var timed <-chan time.Time
	sent := false
	taken := false
	stopTimer := func() {
		if timer == nil {
			return
		}
		if !timer.Stop() {
			select {
			case <-timed:
			default:
			}
		}
		timed = nil
	}
	defer stopTimer()
	pulse := time.NewTicker(10 * time.Millisecond)
	defer pulse.Stop()
	for {
		evt, ok := b.pop(stop)
		if !ok {
			return
		}
		for len(out) > 0 {
			if b.isClosed() && !taken && timed == nil {
				timer = time.NewTimer(idle)
				timed = timer.C
			}
			select {
			case <-timed:
				b.deliverFinal(out, &evt)
				return
			case <-stop:
				return
			case <-pulse.C:
			}
		}
		if sent {
			taken = true
			stopTimer()
		}
		select {
		case out <- evt:
			sent = true
		case <-stop:
			return
		}
	}
}

func (b *eventBox) isClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

func (b *eventBox) deliverFinal(out chan<- agent.Event, held *agent.Event) {
	n, term := b.compact()
	if held != nil && held.Terminal() {
		term = held
	} else if held != nil {
		n++
	}
	if n > 0 {
		select {
		case out <- droppedMarker(n):
		default:
		}
	}
	if term != nil {
		select {
		case out <- *term:
		default:
		}
	}
}

func (b *eventBox) compact() (int, *agent.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var term *agent.Event
	extra := 0
	for i := range b.items {
		if b.items[i].Terminal() {
			evt := b.items[i]
			term = &evt
			continue
		}
		extra++
	}
	b.items = nil
	b.dropped += extra
	n := b.dropped
	b.dropped = 0
	b.closed = true
	return n, term
}

func (b *eventBox) pop(stop <-chan struct{}) (agent.Event, bool) {
	for {
		b.mu.Lock()
		if marker, ok := b.takeDroppedLocked(); ok && (len(b.items) > 0 || b.closed) {
			b.mu.Unlock()
			return marker, true
		}
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
