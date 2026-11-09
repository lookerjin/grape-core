package trpcruntime

import (
	"sync"

	"github.com/lookerjin/grape-core/agent"
)

// eventBox keeps a bounded queue and always retains a terminal event.
// A slow consumer drops the oldest non-terminal events instead of
// blocking the run or losing the terminal event.
type eventBox struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  []agent.Event
	closed bool
	max    int
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
	limit := b.max
	if !evt.Terminal() {
		limit = b.max - 1
	}
	for len(b.items) >= limit {
		if b.dropOldestNonTerminal() {
			continue
		}
		if evt.Terminal() && len(b.items) >= b.max {
			b.items = b.items[1:]
			continue
		}
		break
	}
	if !evt.Terminal() && len(b.items) >= b.max-1 {
		return
	}
	b.items = append(b.items, evt)
	b.cond.Signal()
}

func (b *eventBox) dropOldestNonTerminal() bool {
	for i, item := range b.items {
		if !item.Terminal() {
			b.items = append(b.items[:i], b.items[i+1:]...)
			return true
		}
	}
	return false
}

func (b *eventBox) close() {
	b.mu.Lock()
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
