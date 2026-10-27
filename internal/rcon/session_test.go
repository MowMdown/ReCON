package rcon

import (
	"strings"
	"testing"
	"time"

	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/test"
)

// resetWeb starts a test with no web session and no wait before the
// next login, and closes it and restores the RCON target after the test.
func resetWeb(t *testing.T) {
	oldAddr, oldPass := Target()
	CloseSession()
	t.Cleanup(func() {
		SetTarget(oldAddr, oldPass)
		CloseSession()
	})
}

func TestReconnectWebOnlySwitchesOnSuccess(t *testing.T) {
	resetWeb(t)
	a, b := test.NewServer(t), test.NewServer(t)
	a.SetPassword("apw")
	b.SetPassword("bpw")
	addrA, addrB := a.Addr(), b.Addr()
	SetTarget(addrA, "apw")
	if err := Login(); err != nil {
		t.Fatal(err)
	}
	old, err := liveWebSession()
	if err != nil {
		t.Fatal(err)
	}

	// Wrong password for B: nothing changes.
	if err := Reconnect(addrB, "nope"); err == nil {
		t.Fatal("a rejected login was accepted")
	}
	if addr, _ := Target(); addr != addrA {
		t.Fatalf("target moved to %s after a failed connect", addr)
	}
	if c, _ := liveWebSession(); c != old || old.IsClosed() {
		t.Fatal("the working session was replaced or closed after a failed connect")
	}

	// Right password: B becomes the session and the target; A's session closes.
	if err := Reconnect(addrB, "bpw"); err != nil {
		t.Fatal(err)
	}
	if addr, pass := Target(); addr != addrB || pass != "bpw" {
		t.Fatalf("target = %s %q", addr, pass)
	}
	if c, _ := liveWebSession(); c == old {
		t.Fatal("still on the old session")
	}
	time.Sleep(50 * time.Millisecond)
	if !old.IsClosed() {
		t.Fatal("the old session was left open")
	}
	if _, unlocks := b.Counts(); unlocks != 1 {
		t.Fatalf("B got %d #unlock(s), want 1", unlocks)
	}

	// Connecting again to the same place does a fresh handshake.
	before, _ := b.Counts()
	if err := Reconnect(addrB, "bpw"); err != nil {
		t.Fatal(err)
	}
	if after, _ := b.Counts(); after != before+1 {
		t.Fatalf("reconnecting sent %d login(s), want 1", after-before)
	}
}

func TestNoPasswordDoesNotDial(t *testing.T) {
	if _, err := battleye.Dial("127.0.0.1:1", "", connectOptions(webSource, false)); err != battleye.ErrNoPassword {
		t.Fatalf("err = %v, want errNoPassword", err)
	}
}

// TestConnectTriesOnce checks that RCON Settings' Connect sends one login per
// press, even to a server that doesn't answer, and changes nothing.
func TestConnectTriesOnce(t *testing.T) {
	resetWeb(t)
	oldWait := battleye.ReloginReplyWait
	t.Cleanup(func() { battleye.ReloginReplyWait = oldWait })
	battleye.ReloginReplyWait = 50 * time.Millisecond
	f := test.NewServer(t)
	f.SetSilent(true)
	SetTarget("127.0.0.1:1", "pw")
	for press := 1; press <= 3; press++ {
		if err := Reconnect(f.Addr(), "pw"); err == nil {
			t.Fatal("connected to a server that doesn't answer")
		}
		time.Sleep(20 * time.Millisecond)
		if logins, _ := f.Counts(); logins != press {
			t.Fatalf("after %d press(es): %d login packets, want %d", press, logins, press)
		}
	}
	if addr, _ := Target(); addr != "127.0.0.1:1" {
		t.Fatalf("target changed to %s", addr)
	}
}

// TestSentCommandsGoInConsoleLog checks the master console log: a command
// sent for someone goes in once with who sent it, followed by an error when
// the server doesn't answer; polls and heartbeats don't go in at all; a new
// login logs its #unlock and asks for a players and bans poll.
func TestSentCommandsGoInConsoleLog(t *testing.T) {
	resetWeb(t)
	f := test.NewServer(t)
	SetTarget(f.Addr(), "pw")
	LoginPollDue.Store(false)
	t.Cleanup(func() { LoginPollDue.Store(false) })

	start := Events.LastSeq()
	if err := Login(); err != nil {
		t.Fatal(err)
	}
	if !LoginPollDue.Load() {
		t.Fatal("a new login didn't ask for a poll")
	}
	Run("players", time.Second, false, "")
	Run("say -1 hello", time.Second, false, FlowSource("Greeter"))
	f.SetSilent(true)
	Run("kick 3 bye", 300*time.Millisecond, false, "web")

	var got []string
	for _, ev := range Events.after(start) {
		got = append(got, ev.Kind+"|"+ev.Text+"|"+ev.Source)
	}
	want := []string{
		"sent|> #unlock|login",
		`sent|> say -1 hello|flow "Greeter"`,
		"sent|> kick 3 bye|web",
		"error|(no acknowledgement from server)|web",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("console log:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
