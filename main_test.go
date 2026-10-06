package main

import (
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRcon(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		b := make([]byte, 4096)
		logins := 0
		for {
			n, a, err := pc.ReadFrom(b)
			if err != nil {
				return
			}
			ty, p, ok := parse(b[:n])
			if !ok {
				continue
			}
			switch {
			case ty == 0 && logins == 0:
				logins++ // simulate a lost first login packet
			case ty == 0 && string(p) == "pw":
				pc.WriteTo(build(0, []byte{1}), a)
			case ty == 0:
				pc.WriteTo(build(0, []byte{0}), a)
			case ty == 1 && string(p[1:]) == "say -1 hi":
				pc.WriteTo(build(1, []byte{p[0]}), a) // bare ack, no text
			case ty == 1 && string(p[1:]) == "players":
				pc.WriteTo(build(1, append([]byte{p[0], 0, 2, 0}, "Players:\n"...)), a)
				pc.WriteTo(build(1, append([]byte{p[0], 0, 2, 1}, "(0)"...)), a)
			}
		}
	}()
	rconAddr = pc.LocalAddr().String()
	rconPassword = "pw"
	out, acked, err := rcon("players", time.Second)
	if err != nil || !acked || out != "Players:\n(0)" {
		t.Fatalf("got %q, %v, %v", out, acked, err)
	}
	out, acked, err = rcon("say -1 hi", time.Second)
	if err != nil || !acked || out != "" {
		t.Fatalf("got %q, %v, %v", out, acked, err)
	}
	out, acked, err = rcon("noreply", 300*time.Millisecond)
	if err != nil || acked || out != "" {
		t.Fatalf("got %q, %v, %v", out, acked, err)
	}
	rconPassword = "bad"
	if _, _, err = rcon("players", time.Second); err != errLoginRejected {
		t.Fatalf("expected login rejection, got %v", err)
	}
}

func TestKickAllHighestFirst(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	var mu sync.Mutex
	players := []int{0, 1, 2, 3}
	var kicked []int
	shutdownSeen := false
	go func() {
		b := make([]byte, 4096)
		for {
			n, a, err := pc.ReadFrom(b)
			if err != nil {
				return
			}
			ty, p, ok := parse(b[:n])
			if !ok {
				continue
			}
			if ty == 0 {
				pc.WriteTo(build(0, []byte{1}), a)
				continue
			}
			cmd := string(p[1:])
			mu.Lock()
			switch {
			case cmd == "players":
				var sb strings.Builder
				sb.WriteString("Players on server:\n[#] [IP Address]:[Port] [Ping] [GUID] [Name]\n----------\n")
				for _, id := range players {
					fmt.Fprintf(&sb, "%d   10.0.0.%d:2304   30   abc(OK) Player%d\n", id, id, id)
				}
				fmt.Fprintf(&sb, "(%d players in total)", len(players))
				pc.WriteTo(build(1, append([]byte{p[0]}, sb.String()...)), a)
			case cmd == "#shutdown":
				shutdownSeen = true
				pc.WriteTo(build(1, []byte{p[0]}), a)
			case strings.HasPrefix(cmd, "kick "):
				id, _ := strconv.Atoi(strings.Fields(cmd)[1])
				kicked = append(kicked, id)
				for i, v := range players {
					if v == id {
						players = append(players[:i], players[i+1:]...)
						break
					}
				}
				pc.WriteTo(build(1, []byte{p[0]}), a)
			}
			mu.Unlock()
		}
	}()
	rconAddr = pc.LocalAddr().String()
	rconPassword = "pw"

	rep, err := kickAll("bye", false, true)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []int{3, 2, 1, 0}
	if !reflect.DeepEqual(rep.Order, want) || !reflect.DeepEqual(kicked, want) {
		t.Fatalf("order %v, server saw %v, want %v", rep.Order, kicked, want)
	}
	if !shutdownSeen || !rep.ShutdownSent || !rep.ShutdownAcked {
		t.Fatalf("shutdown seen=%v sent=%v acked=%v", shutdownSeen, rep.ShutdownSent, rep.ShutdownAcked)
	}
	if len(rep.Remaining) != 0 || len(rep.Unacked) != 0 {
		t.Fatalf("remaining %v unacked %v", rep.Remaining, rep.Unacked)
	}
}

func TestPlayerIDsRealFormat(t *testing.T) {
	out := "Players on server:\n" +
		"[#] [IP Address]:[Port] [Ping] [GUID] [Name]\n" +
		"--------------------------------------------------\n" +
		"0   10.0.1.51:41016       -1   49aacf68461f67163d0a2ecb6a4cee94(?)  MowMdown (Lobby)\n" +
		"1   10.0.1.52:41017       35   0123456789abcdef0123456789abcdef(OK)  Some Name\n" +
		"12  10.0.1.53:41018       40   fedcba9876543210fedcba9876543210(OK)  3 Guys 1:2 Pizza\n" +
		"(3 players in total)"
	got := playerIDs(out)
	if !reflect.DeepEqual(got, []int{12, 1, 0}) {
		t.Fatalf("got %v", got)
	}
}

func TestLiveSessionStaysLoggedIn(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	var logins, keepalives, pushAcks int32
	go func() {
		b := make([]byte, 4096)
		pushed := false
		for {
			n, a, err := pc.ReadFrom(b)
			if err != nil {
				return
			}
			ty, p, ok := parse(b[:n])
			if !ok {
				continue
			}
			switch ty {
			case 0:
				atomic.AddInt32(&logins, 1)
				pc.WriteTo(build(0, []byte{1}), a)
			case 1:
				if len(p) == 1 {
					atomic.AddInt32(&keepalives, 1)
					pc.WriteTo(build(1, []byte{p[0]}), a)
					continue
				}
				pc.WriteTo(build(1, append([]byte{p[0]}, "ok"...)), a)
				if !pushed {
					pushed = true
					pc.WriteTo(build(2, []byte{7, 'h', 'i'}), a) // server console message
				}
			case 2:
				if len(p) == 1 && p[0] == 7 {
					atomic.AddInt32(&pushAcks, 1)
				}
			}
		}
	}()
	rconAddr = pc.LocalAddr().String()
	rconPassword = "pw"
	keepaliveEvery = 100 * time.Millisecond

	l, err := dialLive()
	if err != nil {
		t.Fatal(err)
	}
	defer l.close()
	for _, cmd := range []string{"players", "say -1 hi"} {
		out, acked, err := l.run(cmd, time.Second)
		if err != nil || !acked || out != "ok" {
			t.Fatalf("%s: got %q %v %v", cmd, out, acked, err)
		}
	}
	time.Sleep(450 * time.Millisecond)
	if !l.alive() {
		t.Fatal("session should be alive")
	}
	if n := atomic.LoadInt32(&logins); n != 1 {
		t.Fatalf("logged in %d times, want 1", n)
	}
	if n := atomic.LoadInt32(&keepalives); n < 3 {
		t.Fatalf("only %d keepalives", n)
	}
	if n := atomic.LoadInt32(&pushAcks); n != 1 {
		t.Fatalf("server message acked %d times, want 1", n)
	}
}
