package rcon

import (
	"sort"
	"sync"
	"time"

	"github.com/MowMdown/ReCON/internal/battleye"
)

// handleServerMessage puts a web session's server message in the console log.
func handleServerMessage(text string) {
	Events.Publish(battleye.ParseServerMessage(text, time.Now()))
}

// consoleLogLimit is how many lines the master console log (Events)
// keeps: every server message and every command ReCON sent with its reply,
// since ReCON started. Older lines are dropped first.
const consoleLogLimit = 10000

// eventHub is the master console log: it numbers every event, keeps the most
// recent consoleLogLimit of them and fans each one out to subscribers (the
// player tracker, the chat recorder, flows). Safe for concurrent use.
type eventHub struct {
	mu     sync.Mutex
	seq    int64 // Seq of the last published event
	recent []battleye.ServerEvent
	subs   map[chan battleye.ServerEvent]struct{}
}

// Events receives every server message from every session in this
// process (in server mode, only the web session exists).
var Events = &eventHub{subs: map[chan battleye.ServerEvent]struct{}{}}

// Publish numbers an event, records it and hands it to every subscriber
// that has room.
func (h *eventHub) Publish(ev battleye.ServerEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	ev.Seq = h.seq
	h.recent = append(h.recent, ev)
	// Trim only once a tenth over the limit, so the copy happens once per
	// consoleLogLimit/10 events instead of on every one.
	if len(h.recent) >= consoleLogLimit+consoleLogLimit/10 {
		h.recent = append([]battleye.ServerEvent(nil), h.recent[len(h.recent)-consoleLogLimit:]...)
	}
	for ch := range h.subs {
		select {
		case ch <- ev:
		default: // subscriber is behind: it misses this one
		}
	}
}

// subscribe returns a channel of new events (buffered by `buffer`) and a
// cancel func that unsubscribes and closes the channel. Call cancel when done.
func (h *eventHub) Subscribe(buffer int) (<-chan battleye.ServerEvent, func()) {
	ch := make(chan battleye.ServerEvent, buffer)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			h.mu.Unlock()
			close(ch)
		})
	}
}

// lastSeq is the Seq of the newest published event (0 before any).
func (h *eventHub) LastSeq() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seq
}

// recentEvents returns a copy of the kept events, oldest first.
func (h *eventHub) recentEvents() []battleye.ServerEvent {
	return h.after(0)
}

// after returns the kept events with Seq greater than seq, oldest first. A
// page polls with the last value it got; 0 means "everything kept". A seq
// from before a ReCON restart is larger than anything now, so the page gets
// the whole log again.
func (h *eventHub) after(seq int64) []battleye.ServerEvent {
	events, _ := h.AfterWithLast(seq, 0)
	return events
}

// afterWithLast is after, plus the Seq of the newest event (to pass next
// time). A limit above 0 returns only the newest limit events.
func (h *eventHub) AfterWithLast(seq int64, limit int) ([]battleye.ServerEvent, int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if seq > h.seq {
		seq = 0 // ReCON restarted since the caller last asked
	}
	kept := h.recent
	if len(kept) > consoleLogLimit {
		kept = kept[len(kept)-consoleLogLimit:]
	}
	// Seq only grows, so the first newer event can be found by bisection.
	kept = kept[sort.Search(len(kept), func(i int) bool { return kept[i].Seq > seq }):]
	if limit > 0 && len(kept) > limit {
		kept = kept[len(kept)-limit:]
	}
	return append([]battleye.ServerEvent{}, kept...), h.seq
}
