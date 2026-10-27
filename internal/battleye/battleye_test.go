package battleye_test

import (
	"testing"
	"time"

	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/test"
)

func TestParsePlayersUnverifiedGUID(t *testing.T) {
	out := "Players on server:\n" +
		"[#] [IP Address]:[Port] [Ping] [GUID] [Name]\n" +
		"--------------------------------------------------\n" +
		"0   1.2.3.4:2304   47   " + test.GUID1 + "(OK)   Some Name\n" +
		"1   5.6.7.8:2304   0    -   New Guy (Lobby)\n" +
		"(2 players in total)\n"
	ps := battleye.ParsePlayersReply(out)
	if len(ps) != 2 {
		t.Fatalf("parsed %d players, want 2", len(ps))
	}
	if ps[0].GUID != test.GUID1 || ps[0].Name != "Some Name" {
		t.Errorf("player 0 = %+v", ps[0])
	}
	if ps[1].GUID != "" {
		t.Errorf("unverified player's GUID = %q, want empty", ps[1].GUID)
	}
}

// TestGUIDFormats checks the two places a GUID comes from: the `players`
// reply, where BattlEye adds "(OK)" (verified) or "(?)" (not verified yet),
// and the server's GUID line, which has the bare GUID.
func TestGUIDFormats(t *testing.T) {
	out := "Players on server:\n" +
		"[#] [IP Address]:[Port] [Ping] [GUID] [Name]\n" +
		"--------------------------------------------------\n" +
		"0   1.2.3.4:2304   47   " + test.GUID1 + "(OK)   Verified\n" +
		"1   5.6.7.8:2304   60   " + test.GUID2 + "(?)   Pending (Lobby)\n" +
		"(2 players in total)\n"
	ps := battleye.ParsePlayersReply(out)
	if len(ps) != 2 || ps[0].GUID != test.GUID1 || ps[1].GUID != test.GUID2 || ps[1].Name != "Pending (Lobby)" {
		t.Fatalf("players = %+v", ps)
	}
	line := "Player #0 MowMdown - BE GUID: 49aacf68461f67163d0a2ecb6a4cee94"
	ev := battleye.ParseServerMessage(line, time.Now())
	if ev.Kind != battleye.EventGUID || ev.GUID != "49aacf68461f67163d0a2ecb6a4cee94" || ev.PlayerID != 0 || ev.Name != "MowMdown" {
		t.Fatalf("%q = %+v", line, ev)
	}
}
