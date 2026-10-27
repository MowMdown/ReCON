package rcon

import (
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MowMdown/ReCON/internal/battleye"
)

// Dial connects to the current target (Target), logs in,
// and starts the background reader (and the keepalive heartbeat when
// enableKeepalive is true). The web session (webSource) sends one
// login and waits 3s, and puts server messages in the console log; the CLI
// and console try 3 times, 1s apart.
func Dial(src string, enableKeepalive bool) (*battleye.Session, error) {
	address, password := Target()
	return battleye.Dial(address, password, connectOptions(src, enableKeepalive))
}

// connectOptions are the battleye.DialOptions for a session labelled src.
func connectOptions(src string, keepalive bool) battleye.DialOptions {
	if src == webSource {
		return battleye.DialOptions{Source: src, Tries: 1, ReplyWait: battleye.ReloginReplyWait, Keepalive: keepalive, OnMessage: handleServerMessage}
	}
	return battleye.DialOptions{Source: src, Tries: 3, ReplyWait: time.Second, Keepalive: keepalive}
}

// webConnection holds the one long-lived session. The Mutex protects the
// fields; battleye.Session serialises the commands themselves.
// Keepalive is off because the server watch already keeps the session busy.
// Being permanently logged in is also what lets it receive server messages.
//
// Only the server watch logs in (Login, Relogin), at
// most once per heartbeat, and RCON Settings' Connect button
// (Reconnect). Commands never log in: with no live session they
// fail straight away. A server that is starting up can take packets on its
// port before it answers them, and then handles the whole backlog at once,
// so every login sent in that time becomes another "RCon admin logged in".
var webConnection = struct {
	sync.Mutex
	connection *battleye.Session
	lastErr    error     // why the last login failed, for commands sent meanwhile
	loggedInAt time.Time // the last accepted login the server hasn't answered a check after
}{}

// webSource is the web session's label in log lines.
const webSource = "web"

// ErrNotLoggedIn is returned for commands sent while the web session isn't
// logged in; the server watch logs in again at its next heartbeat.
var ErrNotLoggedIn = errors.New("not logged in to RCON")

// liveWebSession returns the web session, or an error when it isn't logged
// in (none yet, or its socket has died). It never logs in itself.
func liveWebSession() (*battleye.Session, error) {
	webConnection.Lock()
	defer webConnection.Unlock()
	if c := webConnection.connection; c != nil && !c.IsClosed() {
		return c, nil
	}
	if c := webConnection.connection; c != nil {
		log.Printf("rcon: connection dropped (socket closed: %v), logging in again at the next heartbeat", c.Err)
		c.Close()
		webConnection.connection = nil
	}
	if err := webConnection.lastErr; err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotLoggedIn, err)
	}
	return nil, ErrNotLoggedIn
}

// Login logs the web session in on a new socket if there is no
// live one, with ONE login packet. Only the server watch calls it, on a
// heartbeat.
//
// Every new login sends #unlock. The web session logs in when ReCON starts
// and after the session drops, most often because the server restarted, so a
// restart flow's #lock never outlives the restart. (A lock set some other way
// is undone if the session reconnects while it's in place; that's accepted.)
// The CLI and console dial their own sessions and don't unlock.
func Login() error {
	if _, err := liveWebSession(); err == nil {
		return nil
	}
	webConnection.Lock()
	defer webConnection.Unlock()
	s, err := Dial(webSource, false)
	if err != nil {
		webConnection.lastErr = err
		return err
	}
	webConnection.connection, webConnection.lastErr, webConnection.loggedInAt = s, nil, time.Now()
	log.Printf("rcon: logged in")
	unlockAfterLogin(s)
	return nil
}

// CloseSession closes the web session, if there is one, and forgets it, so
// the next login opens a new one straight away.
func CloseSession() {
	webConnection.Lock()
	defer webConnection.Unlock()
	if webConnection.connection != nil {
		webConnection.connection.Close()
	}
	webConnection.connection, webConnection.lastErr, webConnection.loggedInAt = nil, nil, time.Time{}
}

// Reconnect logs the web session in to address with password on a
// new socket and, only if the server accepts it, makes that the web
// session and the target for later logins, closing the old session. This is
// RCON Settings' Connect button: a wrong address or password changes
// nothing. Like every new login it sends #unlock and re-reads the ban list.
func Reconnect(address, password string) error {
	webConnection.Lock()
	defer webConnection.Unlock()
	s, err := battleye.Dial(address, password, connectOptions(webSource, false))
	if err != nil {
		return err
	}
	SetTarget(address, password)
	if old := webConnection.connection; old != nil {
		old.Close()
	}
	webConnection.connection, webConnection.lastErr, webConnection.loggedInAt = s, nil, time.Now()
	log.Printf("rcon: connected to %s from RCON Settings", address)
	unlockAfterLogin(s)
	return nil
}

// ReloginSettle is how long after an accepted login no new one is sent: a
// server that is still starting up can accept a login before it answers
// commands, and logging in again then would only add another "logged in"
// line and another #unlock.
var ReloginSettle = 60 * time.Second

// Answered is called by the server watch when the server answers a
// check, so the next outage may log in again straight away.
func Answered() {
	webConnection.Lock()
	webConnection.loggedInAt = time.Time{}
	webConnection.Unlock()
}

// Relogin is called by the server watch, on a heartbeat, while the
// server is down (it may have restarted and forgotten us). It logs in again
// on the same socket, so the server keeps our admin slot, then sends #unlock
// as a new login does, and reports whether the server accepted the login.
// It does nothing within ReloginSettle of an accepted login, or when there
// is no live session (Login opens one).
func Relogin() bool {
	webConnection.Lock()
	c, settling := webConnection.connection, time.Since(webConnection.loggedInAt) < ReloginSettle
	webConnection.Unlock()
	if c == nil || c.IsClosed() || settling {
		return false
	}
	if err := c.Relogin(); err != nil {
		log.Printf("rcon: no reply from the server; logging in again failed (%v), next try at the next heartbeat", err)
		return false
	}
	webConnection.Lock()
	webConnection.loggedInAt = time.Now()
	webConnection.Unlock()
	log.Printf("rcon: no reply from the server; logged in again on the same connection")
	unlockAfterLogin(c)
	return true
}

// loginSource is the logAs label for what ReCON sends by itself after a login.
const loginSource = "login"

// unlockAfterLogin sends #unlock on a session that has just logged in (see
// Login), logs it like any other sent command, and asks the
// server watch to read the ban list next: the server may have restarted,
// so the bans ReCON knew may be out of date. It uses runRawCommand
// because the callers already hold the session.
func unlockAfterLogin(s *battleye.Session) {
	Events.Publish(battleye.ServerEvent{Time: time.Now(), Kind: battleye.EventSent, Text: "> #unlock", Source: loginSource, PlayerID: -1})
	out, acked, err := s.RunRaw("#unlock", 2*time.Second)
	battleye.LogSentCommand(webSource, loginSource, "#unlock", acked, err)
	logCommandResult(loginSource, out, acked, err)
	LoginPollDue.Store(true)
	now := time.Now()
	loginHooks.Lock()
	hooks := loginHooks.fns
	loginHooks.Unlock()
	for _, fn := range hooks {
		fn(now)
	}
}

// loginHooks is what runs after every login of the web session (OnLogin).
var loginHooks struct {
	sync.Mutex
	fns []func(time.Time)
}

// OnLogin adds fn to what runs, with the login time, after every login
// of the web session.
func OnLogin(fn func(time.Time)) {
	loginHooks.Lock()
	loginHooks.fns = append(loginHooks.fns, fn)
	loginHooks.Unlock()
}

// LoginPollDue is set by every login of the web session; the next answered
// check then reads the ban list.
var LoginPollDue atomic.Bool

// Run runs a command on the web session.
//
//   - allowRetry=false: one attempt. Used for state-changing commands (kick,
//     ban, say, ...) so a slow-but-successful command is never repeated.
//   - allowRetry=true: up to 3 attempts with 1s pauses, for read-only
//     commands (players, bans) where repeating is harmless.
//   - logAs: who asked: "web" for actions from the pages and API, "console"
//     for the console page, FlowSource(name) for flow steps. The command,
//     and then its reply or error, go into the master console log
//     (Events) once per call, and each attempt to the container log as
//     "[web] > command (logAs, acked=...)". "" logs nothing: heartbeats,
//     polls and page refreshes.
//
// err is only set for a connection/transport failure. A command the server
// simply didn't acknowledge returns acked=false with err=nil, so callers can
// tell "server never got it" from "server didn't answer" (e.g. #shutdown).
//
// A missed reply never drops the session here: retries reuse it, and only
// the server watch decides the server has stopped answering (see Relogin).
// Commands never log in: while the web session isn't logged in they fail
// with ErrNotLoggedIn, and the server watch logs in at its next heartbeat.
func Run(command string, wait time.Duration, allowRetry bool, logAs string) (string, bool, error) {
	maxAttempts := 1
	if allowRetry {
		maxAttempts = 3
	}
	if logAs != "" {
		Events.Publish(battleye.ServerEvent{Time: time.Now(), Kind: battleye.EventSent, Text: "> " + command, Source: logAs, PlayerID: -1})
	}
	out, acked, err := runWebAttempts(command, wait, maxAttempts, logAs)
	if logAs != "" {
		logCommandResult(logAs, out, acked, err)
	}
	return out, acked, err
}

// logCommandResult adds what came back for a sent command to the master
// console log: its output, the error, or that the server never answered.
// An acknowledged command with no output adds nothing.
func logCommandResult(logAs, out string, acked bool, err error) {
	note := func(kind, text string) {
		Events.Publish(battleye.ServerEvent{Time: time.Now(), Kind: kind, Text: text, Source: logAs, PlayerID: -1})
	}
	switch {
	case err != nil:
		note(battleye.EventError, "error: "+err.Error())
	case !acked:
		note(battleye.EventError, "(no acknowledgement from server)")
	case strings.TrimSpace(out) != "":
		note(battleye.EventReply, strings.TrimRight(out, "\r\n "))
	}
}

// FlowSource is the Run logAs label for a flow's commands.
func FlowSource(name string) string {
	return "flow " + strconv.Quote(name)
}

// runWebAttempts is Run's send loop: up to maxAttempts tries with
// 1s pauses between them.
func runWebAttempts(command string, wait time.Duration, maxAttempts int, logAs string) (string, bool, error) {
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		more := attempt+1 < maxAttempts // only pause when another attempt follows
		s, err := liveWebSession()
		if err != nil {
			lastErr = err
			if more {
				time.Sleep(1 * time.Second)
			}
			continue
		}
		out, acked, err := s.RunRaw(command, wait)
		if logAs != "" {
			battleye.LogSentCommand(webSource, logAs, command, acked, err)
		}
		if err == nil && acked {
			return out, true, nil
		}
		lastErr = err // nil when the command was merely unacknowledged
		if more {
			time.Sleep(1 * time.Second)
		}
	}
	return "", false, lastErr
}

// webRunner adapts Run to the commandRunner interface (single
// attempt per command) for battleye.KickAll. The kicks are logged as logAs; the
// `players` reads around them are polls and aren't.
type webRunner struct{ logAs string }

func (r webRunner) Run(command string, wait time.Duration) (string, bool, error) {
	if command == "players" {
		return Run(command, wait, false, "")
	}
	return Run(command, wait, false, r.logAs)
}

// KickAll runs ReCON's own kickall command on the web session for
// logAs (see Run): "> kickall <reason>" and then its summary go into
// the master console log and the container log, with each kick between.
func KickAll(reason, logAs string) (*battleye.KickReport, error) {
	if reason = battleye.CleanReason(reason); reason == "" {
		reason = "Server restarting"
	}
	Events.Publish(battleye.ServerEvent{Time: time.Now(), Kind: battleye.EventSent, Text: "> kickall " + reason, Source: logAs, PlayerID: -1})
	rep, err := battleye.KickAll(webRunner{logAs}, reason)
	if err != nil {
		logCommandResult(logAs, "", false, fmt.Errorf("kickall failed: %w", err))
		return nil, err
	}
	log.Printf("[%s] > %s (%s)", webSource, rep.Summary(reason), logAs)
	logCommandResult(logAs, rep.Summary(reason), true, nil)
	return rep, nil
}

// ConsoleSource is the Run logAs label for commands typed in the
// console page; only those are echoed into its log.
const ConsoleSource = "console"
