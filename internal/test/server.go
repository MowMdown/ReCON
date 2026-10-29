// Package test has everything ReCON's tests share: a fake BattlEye RCON
// server (Server), a throwaway player database (OpenDB) with helpers to
// read it back, and two valid GUIDs. Only _test.go files import it, so none
// of it is built into the program.
//
// It may only import low-level packages (battleye, store): a package's own
// tests can't import a package that imports it back.
package test

import (
	"net"
	"sync"
	"testing"

	"github.com/MowMdown/ReCON/internal/battleye"
)

// Server is a minimal BattlEye RCON server on localhost that counts the
// login packets and commands it gets, and can be told to stop answering.
// It acknowledges every command with an empty reply.
type Server struct {
	conn     net.PacketConn
	mu       sync.Mutex
	silent   bool       // take packets but don't answer, like a server still starting
	holding  bool       // like silent, but keep the logins and answer them on Release
	held     []net.Addr // where the held logins came from
	mute     bool   // answer logins but not commands, like a server that's nearly up
	password string // when set, other passwords are rejected
	logins   int
	commands []string
	beats    int // empty commands (heartbeats)
}

// NewServer starts a fake server, stopped when the test ends.
func NewServer(t testing.TB) *Server {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &Server{conn: c}
	t.Cleanup(func() { c.Close() })
	go f.serve()
	return f
}

func (f *Server) serve() {
	buf := make([]byte, 4096)
	for {
		n, from, err := f.conn.ReadFrom(buf)
		if err != nil {
			return
		}
		typ, p, ok := battleye.ParsePacket(buf[:n])
		if !ok {
			continue
		}
		f.mu.Lock()
		silent, accept := f.silent || f.holding || (f.mute && typ == 1), f.password == "" || string(p) == f.password
		switch typ {
		case 0:
			f.logins++
			if f.holding {
				f.held = append(f.held, from)
			}
		case 1:
			if len(p) > 1 {
				f.commands = append(f.commands, string(p[1:]))
			} else {
				f.beats++
			}
		}
		f.mu.Unlock()
		if silent {
			continue
		}
		switch typ {
		case 0:
			ok := byte(0)
			if accept {
				ok = 1
			}
			f.conn.WriteTo(battleye.BuildPacket(0, []byte{ok}), from)
		case 1:
			f.conn.WriteTo(battleye.BuildPacket(1, p[:1]), from) // ack with the same seq
		}
	}
}

// Close stops the server: its port is closed, so packets sent to it are
// refused, as when the game server has shut down.
func (f *Server) Close() { f.conn.Close() }

// Addr is the server's address, "127.0.0.1:port".
func (f *Server) Addr() string { return f.conn.LocalAddr().String() }

// SetSilent makes the server take packets without answering any.
func (f *Server) SetSilent(silent bool) { f.mu.Lock(); f.silent = silent; f.mu.Unlock() }

// Hold makes the server take packets without answering any, like BattlEye
// while the game server starts, keeping the logins it gets for Release.
func (f *Server) Hold() { f.mu.Lock(); f.holding = true; f.mu.Unlock() }

// Release answers the logins taken while holding, as BattlEye does once it
// is up, and answers everything again.
func (f *Server) Release() {
	f.mu.Lock()
	held := f.held
	f.holding, f.held = false, nil
	f.mu.Unlock()
	for _, to := range held {
		f.conn.WriteTo(battleye.BuildPacket(0, []byte{1}), to)
	}
}

// SetMute makes the server answer logins but not commands.
func (f *Server) SetMute(mute bool) { f.mu.Lock(); f.mute = mute; f.mu.Unlock() }

// SetPassword makes the server reject logins with any other password.
func (f *Server) SetPassword(pw string) { f.mu.Lock(); f.password = pw; f.mu.Unlock() }

// Counts is how many login packets and #unlock commands it has got.
func (f *Server) Counts() (logins, unlocks int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.commands {
		if c == "#unlock" {
			unlocks++
		}
	}
	return f.logins, unlocks
}

// Commands is every non-empty command it has got, oldest first.
func (f *Server) Commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}

// Beats is how many empty commands (heartbeats) it has got.
func (f *Server) Beats() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.beats
}
