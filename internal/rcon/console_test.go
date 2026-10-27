package rcon

import (
	"testing"

	"github.com/MowMdown/ReCON/internal/battleye"
)

func TestEventHubKeepsNewest(t *testing.T) {
	h := &eventHub{subs: map[chan battleye.ServerEvent]struct{}{}}
	total := 3*consoleLogLimit + 7
	for i := 0; i < total; i++ {
		h.Publish(battleye.ServerEvent{Kind: battleye.EventOther})
	}
	ev := h.recentEvents()
	if len(ev) != consoleLogLimit {
		t.Fatalf("kept %d events, want %d", len(ev), consoleLogLimit)
	}
	if last := ev[len(ev)-1].Seq; last != int64(total) {
		t.Fatalf("newest Seq = %d", last)
	}
	if first := ev[0].Seq; first != int64(total-consoleLogLimit+1) {
		t.Fatalf("oldest Seq = %d", first)
	}
	// after() finds its place by bisection; a limit keeps the newest.
	if got := h.after(int64(total - 3)); len(got) != 3 || got[0].Seq != int64(total-2) {
		t.Fatalf("after(total-3) = %d events starting at %d", len(got), got[0].Seq)
	}
	got, last := h.AfterWithLast(0, 10)
	if len(got) != 10 || got[9].Seq != int64(total) || last != int64(total) {
		t.Fatalf("afterWithLast(0, 10) = %d events, last %d", len(got), last)
	}
}
