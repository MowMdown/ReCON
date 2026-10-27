package steam

import (
	"testing"
	"time"
)

func TestSteamBatchTiming(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	due := steamBatchDue(t0, time.Time{}, 1, t0)
	if !due.Equal(t0.Add(time.Minute)) {
		t.Fatalf("first join: due %v, want a minute later", due)
	}
	due = steamBatchDue(t0, due, 2, t0.Add(10*time.Second))
	if !due.Equal(t0.Add(90 * time.Second)) {
		t.Fatalf("second join: due %v, want 30s more", due)
	}
	for n := 3; n <= 10; n++ {
		due = steamBatchDue(t0, due, n, t0.Add(20*time.Second))
	}
	if !due.Equal(t0.Add(3 * time.Minute)) {
		t.Fatalf("many joins: due %v, want capped at 3 minutes", due)
	}
	now := t0.Add(30 * time.Second)
	if due := steamBatchDue(t0, due, steamBatchMaxPlayers, now); !due.Equal(now) {
		t.Fatalf("%d players: due %v, want now", steamBatchMaxPlayers, due)
	}
}
