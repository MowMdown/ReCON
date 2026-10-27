package battleye

import (
	"testing"
)

func TestParsePlayersUnverifiedGUID(t *testing.T) {
	out := "Players on server:\n" +
		"[#] [IP Address]:[Port] [Ping] [GUID] [Name]\n" +
		"--------------------------------------------------\n" +
		"0   1.2.3.4:2304   47   " + testGUID + "(OK)   Some Name\n" +
		"1   5.6.7.8:2304   0    -   New Guy (Lobby)\n" +
		"(2 players in total)\n"
	ps := ParsePlayersReply(out)
	if len(ps) != 2 {
		t.Fatalf("parsed %d players, want 2", len(ps))
	}
	if ps[0].GUID != testGUID || ps[0].Name != "Some Name" {
		t.Errorf("player 0 = %+v", ps[0])
	}
	if ps[1].GUID != "" {
		t.Errorf("unverified player's GUID = %q, want empty", ps[1].GUID)
	}
}

const testGUID = "0123456789abcdef0123456789abcdef"
