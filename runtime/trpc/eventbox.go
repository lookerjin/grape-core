package trpcruntime

import (
	"context"
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

// pump sends queued events until the box is drained, stop is closed, or
// the caller context ends. hold is how long a finished run waits for a
// consumer who has not taken an event. idle is how long it waits after
// the last take. Either expiry keeps the terminal event and exits.
// Cancel drops the non-terminal backlog the same way.
func (b *eventBox) pump(out chan<- agent.Event, stop <-chan struct{}, ctx context.Context, hold, idle time.Duration) {
	defer close(out)
	if hold <= 0 {
		hold = 30 * time.Second
	}
	if idle <= 0 {
		idle = 5 * time.Minute
	}
	done := ctxDone(ctx)
	var timer *time.Timer
	var timed <-chan time.Time
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
	sent := false
	var lastTake time.Time
	pulse := time.NewTicker(10 * time.Millisecond)
	defer pulse.Stop()
	finish := func(held *agent.Event) {
		if !b.deliverFinal(out, held) {
			select {
			case out <- cancelledTerminal(ctx):
			default:
			}
		}
	}
	for {
		evt, ok := b.pop(stop, done)
		if !ok {
			if ctx != nil && ctx.Err() != nil {
				finish(nil)
			}
			return
		}
		for len(out) > 0 {
			if b.isClosed() && timed == nil {
				wait := hold
				if !lastTake.IsZero() {
					wait = idle - time.Since(lastTake)
				}
				if wait <= 0 {
					finish(&evt)
					return
				}
				timer = time.NewTimer(wait)
				timed = timer.C
			}
			select {
			case <-timed:
				finish(&evt)
				return
			case <-done:
				if b.isClosed() {
					finish(&evt)
					return
				}
			case <-stop:
				return
			case <-pulse.C:
			}
		}
		if sent {
			lastTake = time.Now()
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

func ctxDone(ctx context.Context) <-chan struct{} {
	if ctx == nil {
		return nil
	}
	return ctx.Done()
}

func cancelledTerminal(ctx context.Context) agent.Event {
	msg := "context canceled"
	if ctx != nil && ctx.Err() != nil {
		msg = ctx.Err().Error()
	}
	return agent.Event{
		Kind: agent.EventCancelled,
		Time: time.Now().UTC(),
		Payload: agent.MustPayload(agent.TerminalPayload{
			Status: string(agent.EventCancelled),
			Error:  msg,
		}),
	}
}

func (b *eventBox) isClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

func keepOnCancel(evt agent.Event) bool {
	return evt.Terminal() || evt.Kind == agent.EventToolEnd
}

func (b *eventBox) deliverFinal(out chan<- agent.Event, held *agent.Event) bool {
	n, kept := b.compact()
	if held != nil && keepOnCancel(*held) {
		kept = append(kept, *held)
	} else if held != nil {
		n++
	}
	if n > 0 {
		select {
		case out <- droppedMarker(n):
		default:
		}
	}
	sentTerminal := false
	for _, evt := range kept {
		select {
		case out <- evt:
			if evt.Terminal() {
				sentTerminal = true
			}
		default:
		}
	}
	return sentTerminal
}

func (b *eventBox) compact() (int, []agent.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var kept []agent.Event
	extra := 0
	for i := range b.items {
		if keepOnCancel(b.items[i]) {
			kept = append(kept, b.items[i])
			continue
		}
		extra++
	}
	b.items = nil
	b.dropped += extra
	n := b.dropped
	b.dropped = 0
	b.closed = true
	return n, kept
}

func (b *eventBox) pop(stop <-chan struct{}, done <-chan struct{}) (agent.Event, bool) {
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
		case <-done:
			if b.isClosed() {
				return agent.Event{}, false
			}
			// The run has not closed the box yet. Wait for its final events.
			select {
			case <-b.wake:
			case <-stop:
				return agent.Event{}, false
			}
		}
	}
}
