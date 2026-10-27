package rcon

import (
	"sync/atomic"
	"time"
)

// DownAfterMisses is how many checks (polls or heartbeats) in a row may go
// unanswered before the server is taken to be down.
const DownAfterMisses = 3

// What the checks saw, for the status bar (Status).
var (
	Polled atomic.Bool  // the server has been checked at least once
	Fails  atomic.Int32 // unanswered checks in a row (RCON Settings' Connect resets it)
	// NextPoll is when ReCON checks the game server next (Unix
	// milliseconds), for the status bar's retry countdown (NextCheck).
	NextPoll atomic.Int64
)

// Status is the game server connection as the server watch (package watch)
// last saw it: connected, retrying (one or two failed checks, or none done
// yet), or unavailable (3 or more failed checks in a row, the point where the
// server watch treats the server as down).
func Status() string {
	switch f := Fails.Load(); {
	case !Polled.Load():
		return "retrying"
	case f == 0:
		return "connected"
	case f >= DownAfterMisses:
		return "unavailable"
	}
	return "retrying"
}

// NextCheck is when ReCON next tries the game server: the server watch's
// next check (its next heartbeat while the server is down).
func NextCheck() time.Time {
	return time.UnixMilli(NextPoll.Load())
}
