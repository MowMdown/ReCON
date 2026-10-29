// Package game is ReCON's link to the game server and the state it mirrors
// from it.
//
//   - The web session (session.go): one logged-in battleye.Session, shared by
//     the pages, flows and polls. Run sends a command and logs it, and its
//     reply, to the console log (Events) and the container log as [web].
//     Every server message and every command ReCON sends goes in the console
//     log; the player tracker, chat history and flows all work from it.
//     Settings say where the server is (RCON_HOST, RCON_PORT, RCON_PASSWORD),
//     overridden in memory by RCON Settings.
//
//   - The server watch (StartWatch, session.go): every 5 seconds it polls
//     `players` while the server answers, and every 30 seconds it sends the
//     heartbeat, an empty command that keeps the web session alive. The
//     heartbeat is the only thing that logs in: on a new socket only when the
//     old one has died, otherwise again on the same socket, so BattlEye keeps
//     the same admin slot. A login the server doesn't answer straight away
//     stays pending, and no other is sent while it waits: a starting server
//     answers it once BattlEye is up, and ReCON is connected from then on.
//     Three checks in a row without an answer mark the server down. See
//     docs/rcon-login.md.
//
//   - Players (players.go): who is online and their sessions, from the
//     console log:
//
//     Player #3 Name (1.2.3.4:2304) connected        -> joining: number, name, IP, time
//     Player #3 Name - BE GUID: <guid>               -> GUID known: the session opens
//     Verified GUID (<guid>) of player #3 Name          (whichever line comes first)
//     `players` poll without "(Lobby)" in the name   -> finished loading
//     Player #3 Name (<guid>) has been kicked by ... -> the session will end as kicked
//     Player #3 Name disconnected                    -> the session closes
//     (Global) Name: text                            -> kept with the player for
//     this session (memory only)
//
//     The database is only written when one of those happens, never for a
//     player who simply stays online. Playtime counts from the connect line,
//     so loading time is included. A `players` poll (PollPlayers) fills in
//     what the messages can't tell and catches anything the console log
//     missed. When the server stops answering, every session is closed at
//     the time it last answered (ServerWentDown) but remembered; a player
//     who comes back from the same IP within restartGrace of the next login
//     keeps the same session. Joins and new sessions go to hooks set at
//     startup (SetPlayerHooks): the join flows and the SteamID lookup.
//
//   - Bans (bans.go): ReCON's ban list kept in step with BattlEye's. It reads
//     the server's list (SyncBans) after every login and every BanSyncEvery,
//     puts back permanent bans the server has lost, lets timed bans run out
//     (ExpireBans), places bans (BanGUID) and exports and imports permanent
//     bans. Everything that changes or mirrors the ban list holds
//     BanListLock, so a sync never acts on a stale list.
package game

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/store"
)

// environmentOrDefault returns the value of the environment variable `key`,
// or `def` when the variable is unset or empty.
func environmentOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Settings is where ReCON finds the game server's RCON. The defaults
// come from the environment (docker-compose.yaml): RCON_HOST (default
// 127.0.0.1), RCON_PORT (default 2306) and RCON_PASSWORD. RCON Settings on
// the Players page can override any of them until ReCON restarts: the
// overrides are kept in memory only (rconOverrides), and a field left empty
// uses the default. The CLI uses the environment only.
type Settings struct {
	Host     string `json:"host"`
	Port     string `json:"port"`
	Password string `json:"password"`
}

// rconOverrides is what RCON Settings last connected with (all empty until
// then).
var rconOverrides struct {
	sync.Mutex
	settings Settings
}

// DefaultSettings are the settings from the environment.
func DefaultSettings() Settings {
	return Settings{
		Host:     environmentOrDefault("RCON_HOST", "127.0.0.1"),
		Port:     environmentOrDefault("RCON_PORT", "2306"),
		Password: os.Getenv("RCON_PASSWORD"),
	}
}

// clean trims the fields and checks them. Empty fields are fine (they use
// the defaults). Errors are user-facing.
func (s Settings) Clean() (Settings, error) {
	s.Host, s.Port = strings.TrimSpace(s.Host), strings.TrimSpace(s.Port)
	if strings.ContainsAny(s.Host, " \t/") {
		return s, errors.New("Server IP: enter an IP address or host name, such as 10.0.0.5")
	}
	if s.Port != "" {
		if n, err := strconv.Atoi(s.Port); err != nil || n < 1 || n > 65535 {
			return s, errors.New("Port: enter a number from 1 to 65535")
		}
	}
	return s, nil
}

// effective fills the empty fields from the defaults and returns the RCON
// endpoint ("host:port") and password to use.
func (s Settings) Effective() (address, password string) {
	d := DefaultSettings()
	if s.Host == "" {
		s.Host = d.Host
	}
	if s.Port == "" {
		s.Port = d.Port
	}
	if s.Password == "" {
		s.Password = d.Password
	}
	return net.JoinHostPort(s.Host, s.Port), s.Password
}

// CurrentOverrides is what RCON Settings last connected with.
func CurrentOverrides() Settings {
	rconOverrides.Lock()
	defer rconOverrides.Unlock()
	return rconOverrides.settings
}

// KeepOverrides remembers the settings RCON Settings connected with,
// until ReCON restarts.
func KeepOverrides(s Settings) {
	rconOverrides.Lock()
	rconOverrides.settings = s
	rconOverrides.Unlock()
}

// rconTarget is the game server's RCON endpoint ("host:port") and password:
// the docker-compose settings, or what was saved in RCON Settings on the
// Players page (Settings). It changes while ReCON runs when
// RCON Settings connects somewhere new, so it is read under its lock.
var rconTarget struct {
	sync.RWMutex
	address, password string
}

// Target returns the RCON endpoint and password new sessions use.
func Target() (address, password string) {
	rconTarget.RLock()
	defer rconTarget.RUnlock()
	return rconTarget.address, rconTarget.password
}

// SetTarget changes the RCON endpoint and password new sessions use.
func SetTarget(address, password string) {
	rconTarget.Lock()
	rconTarget.address, rconTarget.password = address, password
	rconTarget.Unlock()
}

// Dial connects to the current target (Target), logs in,
// and starts the background reader (and the keepalive heartbeat when
// enableKeepalive is true). The web session (webSource) sends one
// login and waits 3s, then keeps the session waiting for the answer
// (battleye.ErrLoginPending), and puts server messages in the console log;
// the CLI and console try 3 times, 1s apart.
func Dial(src string, enableKeepalive bool) (*battleye.Session, error) {
	address, password := Target()
	return battleye.Dial(address, password, connectOptions(src, enableKeepalive))
}

// connectOptions are the battleye.DialOptions for a session labelled src.
func connectOptions(src string, keepalive bool) battleye.DialOptions {
	if src == webSource {
		return battleye.DialOptions{Source: src, Tries: 1, ReplyWait: battleye.ReloginReplyWait, KeepWaiting: true, Keepalive: keepalive, OnMessage: handleServerMessage, OnLogin: signalLogin}
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
	connection *battleye.Session // logged in, or waiting for its login to be answered
	lastErr    error             // why the last login failed, for commands sent meanwhile
	loggedInAt time.Time         // the last accepted login the server hasn't answered a check after
	handled    uint64            // the connection's accepted logins afterLogin has run for
}{}

// webSource is the web session's label in log lines.
const webSource = "web"

// ErrNotLoggedIn is returned for commands sent while the web session isn't
// logged in; the server watch logs in again at its next heartbeat.
var ErrNotLoggedIn = errors.New("not logged in to RCON")

// liveWebSession returns the web session, or an error when it isn't logged
// in (none yet, its socket has died, or its login hasn't been answered yet).
// It never logs in itself.
func liveWebSession() (*battleye.Session, error) {
	webConnection.Lock()
	defer webConnection.Unlock()
	if c := webConnection.connection; c != nil && !c.IsClosed() {
		if !c.LoggedIn() {
			return nil, fmt.Errorf("%w: %v", ErrNotLoggedIn, battleye.ErrLoginPending)
		}
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

// LoginResendAfter is how long a login may go unanswered before the same
// login is sent again on the same socket, in case the packet itself was
// lost. A starting server answers well within it (37s in a test against
// DayZ).
var LoginResendAfter = 5 * time.Minute

// Login makes sure the web session is logged in, or waiting for its login
// to be answered. Only the server watch calls it, on a heartbeat, and it
// sends at most ONE login packet:
//
//   - No live socket (none yet, or the server refused our packets because its
//     port closed): log in on a new socket. A login that isn't answered
//     within 3s stays pending on that socket.
//   - A login pending: send nothing but an empty command (Probe), which
//     closes the session if the port has gone away (that login is lost).
//     After LoginResendAfter, send the login again on the same socket.
//   - Logged out on a live socket (the login was rejected): log in again on it.
//
// It returns nil when logged in and battleye.ErrLoginPending while waiting.
// Every accepted login sends #unlock (afterLogin), even one answered late.
func Login() error {
	if _, err := liveWebSession(); err == nil {
		return nil
	}
	webConnection.Lock()
	c := webConnection.connection
	webConnection.Unlock()
	if c != nil {
		if since, ok := c.LoginPending(); ok && time.Since(since) < LoginResendAfter {
			c.Probe()
			return battleye.ErrLoginPending
		}
		err := c.Relogin()
		webConnection.Lock()
		webConnection.lastErr = err
		webConnection.Unlock()
		if err == nil {
			log.Printf("rcon: logged in")
			afterLogin(c)
		}
		return err
	}
	s, err := Dial(webSource, false)
	if s == nil {
		webConnection.Lock()
		webConnection.lastErr = err
		webConnection.Unlock()
		return err
	}
	webConnection.Lock()
	webConnection.connection, webConnection.lastErr, webConnection.handled = s, err, 0
	webConnection.Unlock()
	if err != nil {
		log.Printf("rcon: login sent, waiting for the server to answer it (no new login meanwhile)")
		return err
	}
	log.Printf("rcon: logged in")
	afterLogin(s)
	return nil
}

// WaitingForLogin reports whether the web session has sent a login the
// server hasn't answered yet. No other login goes out meanwhile, so the
// status bar shows no countdown.
func WaitingForLogin() bool {
	webConnection.Lock()
	c := webConnection.connection
	webConnection.Unlock()
	if c == nil || c.IsClosed() {
		return false
	}
	_, ok := c.LoginPending()
	return ok
}

// afterLogin runs once for each login the server accepts on the web
// session, whether it was answered at once or late: it sends #unlock
// (unlockAfterLogin). ran is false when it had already run for that login
// (or s is no longer the web session); acked is whether the server
// acknowledged #unlock.
func afterLogin(s *battleye.Session) (ran, acked bool) {
	webConnection.Lock()
	if s != webConnection.connection || s.Logins() <= webConnection.handled {
		webConnection.Unlock()
		return false, false
	}
	webConnection.handled = s.Logins()
	webConnection.lastErr, webConnection.loggedInAt = nil, time.Now()
	webConnection.Unlock()
	return true, unlockAfterLogin(s)
}

// loginAnswered wakes the server watch when a login is accepted
// (signalLogin), so a login answered late shows as connected at once.
var loginAnswered = make(chan struct{}, 1)

// signalLogin is the web session's OnLogin: it runs on the session's reader,
// so it only signals the server watch.
func signalLogin() {
	select {
	case loginAnswered <- struct{}{}:
	default: // already signalled
	}
}

// CloseSession closes the web session, if there is one, and forgets it, so
// the next login opens a new one straight away.
func CloseSession() {
	webConnection.Lock()
	defer webConnection.Unlock()
	if webConnection.connection != nil {
		webConnection.connection.Close()
	}
	webConnection.connection, webConnection.lastErr, webConnection.loggedInAt, webConnection.handled = nil, nil, time.Time{}, 0
}

// Reconnect logs the web session in to address with password on a
// new socket and, only if the server accepts it, makes that the web
// session and the target for later logins, closing the old session. This is
// RCON Settings' Connect button: a wrong address or password changes
// nothing, and so does a server that doesn't answer within 3s. Like every
// new login it sends #unlock and re-reads the ban list.
func Reconnect(address, password string) error {
	opt := connectOptions(webSource, false)
	opt.KeepWaiting = false
	s, err := battleye.Dial(address, password, opt)
	if err != nil {
		return err
	}
	SetTarget(address, password)
	webConnection.Lock()
	if old := webConnection.connection; old != nil {
		old.Close()
	}
	webConnection.connection, webConnection.lastErr, webConnection.handled = s, nil, 0
	webConnection.Unlock()
	log.Printf("rcon: connected to %s from RCON Settings", address)
	afterLogin(s)
	return nil
}

// ReloginSettle is how long after an accepted login no new one is sent: a
// server that is still starting up can accept a login before it answers
// commands, and logging in again then would only add another "logged in"
// line and another #unlock.
var ReloginSettle = 60 * time.Second

// markAnswered is called by the server watch when the server answers a
// check, so the next outage may log in again straight away.
func markAnswered() {
	webConnection.Lock()
	webConnection.loggedInAt = time.Time{}
	webConnection.Unlock()
}

// Relogin is called by the server watch, on a heartbeat, while the
// server is down (it may have restarted and forgotten us). It logs in again
// on the same socket, so the server keeps our admin slot, then sends #unlock
// as a new login does, and reports whether the server accepted the login.
// A login it doesn't answer within 3s stays pending: Login sends no other
// while it waits, and the watch takes the answer when it comes.
// It does nothing within ReloginSettle of an accepted login, or when there
// is no live session (Login opens one).
func Relogin() bool {
	webConnection.Lock()
	c, settling := webConnection.connection, time.Since(webConnection.loggedInAt) < ReloginSettle
	webConnection.Unlock()
	if c == nil || c.IsClosed() || settling {
		return false
	}
	err := c.Relogin()
	switch {
	case errors.Is(err, battleye.ErrLoginPending):
		log.Printf("rcon: no reply from the server; login sent on the same connection, waiting for the server to answer it")
		return false
	case err != nil:
		log.Printf("rcon: no reply from the server; logging in again failed (%v), next try at the next heartbeat", err)
		return false
	}
	log.Printf("rcon: no reply from the server; logged in again on the same connection")
	afterLogin(c)
	return true
}

// loginSource is the logAs label for what ReCON sends by itself after a login.
const loginSource = "login"

// unlockAfterLogin sends #unlock on a session that has just logged in (see
// Login), logs it like any other sent command, and asks the
// server watch to read the ban list next: the server may have restarted,
// so the bans ReCON knew may be out of date. It uses runRawCommand
// because the callers already hold the session. It reports whether the
// server acknowledged #unlock.
func unlockAfterLogin(s *battleye.Session) bool {
	Events.Publish(battleye.ServerEvent{Time: time.Now(), Kind: battleye.EventSent, Text: "> #unlock", Source: loginSource, PlayerID: -1})
	out, acked, err := s.RunRaw("#unlock", 2*time.Second)
	battleye.LogSentCommand(webSource, loginSource, "#unlock", acked, err)
	logCommandResult(loginSource, out, acked, err)
	loginPollDue.Store(true)
	now := time.Now()
	loginHooks.Lock()
	hooks := loginHooks.fns
	loginHooks.Unlock()
	for _, fn := range hooks {
		fn(now)
	}
	return err == nil && acked
}

// loginHooks is what runs after every login of the web session (OnLogin).
var loginHooks struct {
	sync.Mutex
	fns []func(time.Time)
}

// onLogin adds fn to what runs, with the login time, after every login
// of the web session.
func onLogin(fn func(time.Time)) {
	loginHooks.Lock()
	loginHooks.fns = append(loginHooks.fns, fn)
	loginHooks.Unlock()
}

// loginPollDue is set by every login of the web session; the next answered
// check then reads the ban list.
var loginPollDue atomic.Bool

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

// pollEvery is how often `players` is polled while the server is answering.
// The poll keeps the player tracker (package players) up to date: pings, and anyone the
// console log missed leaving is closed within one poll.
var pollEvery = 5 * time.Second

// heartbeatEvery is how often an empty command is sent, whether or not the
// server is answering. BattlEye drops a client it hasn't heard from in about
// 45 seconds. The heartbeat is also the only thing that logs in, so a server
// that isn't answering gets at most one login packet per heartbeatEvery;
// while it's down, the heartbeat is the only check (polls pause).
var heartbeatEvery = 30 * time.Second

var heartbeatWait = 2 * time.Second

// StartWatch checks the web session every pollEvery, starting now:
//
//   - Delete timed bans that have run out, if one is due (ExpireDue), and let go
//     of players who didn't come back after a restart in time
//     (ForgetAway).
//   - Every heartbeatEvery, the heartbeat: log in on a new socket if there is
//     no live session (Login), then send an empty command
//     (heartbeatWait). While the server is down and the empty command goes
//     unanswered, log in again on the same socket (Relogin). A login
//     waiting for its answer gets no other until it is answered.
//   - As soon as the server answers a login late, a round runs
//     (loginLanded): #unlock and a `players` poll, and the status shows
//     connected.
//   - While the server is answering, poll `players` (PollNow).
//     Neither is logged.
//   - A round with no answer counts as a miss. At exactly DownAfterMisses in
//     a row the server is taken to be down: open sessions are closed once
//     (ServerWentDown) so playtime stops while it's offline. From
//     then on only the heartbeat checks it. This is the only place that
//     decides the server isn't answering.
//   - An answered round resets the count, and reads the ban list after a
//     login and every SyncEvery.
func StartWatch() {
	go func() {
		var w serverWatch
		w.check(time.Now())
		t := time.NewTicker(pollEvery)
		defer t.Stop()
		for {
			select {
			case now := <-t.C:
				w.check(now)
			case <-loginAnswered:
				w.loginLanded(time.Now())
			}
		}
	}()
}

// serverWatch is what the watch goroutine remembers between rounds.
type serverWatch struct {
	lastAnswered  time.Time // when the server last answered
	lastHeartbeat time.Time // when the last heartbeat was sent
	lastBanSync   time.Time // when the ban list was last read
}

// isDown reports whether the server has missed DownAfterMisses checks in a row.
func (w *serverWatch) isDown() bool { return Fails.Load() >= DownAfterMisses }

// check is one round (see Start).
func (w *serverWatch) check(now time.Time) {
	ExpireBans(now)
	ForgetAway(now)
	defer w.storeNextCheck(now)
	heartbeatDue := now.Sub(w.lastHeartbeat) >= heartbeatEvery
	if w.isDown() && !heartbeatDue {
		return // only the heartbeat checks a server that's down
	}
	answered := false
	if heartbeatDue {
		w.lastHeartbeat = now
		answered = w.heartbeat()
	}
	if !w.isDown() {
		answered = PollPlayers() || answered
	}
	Polled.Store(true)
	if !answered {
		if Fails.Add(1) == DownAfterMisses {
			down := w.lastAnswered
			if down.IsZero() {
				down = now
			}
			ServerWentDown(down)
		}
		return
	}
	w.answered(now)
}

// loginLanded runs when the server accepts a login (signalLogin). For a
// login the heartbeat stopped waiting for, it sends #unlock (afterLogin)
// and polls `players`; either answered makes the status connected. A login
// accepted while the heartbeat waited was already handled there.
func (w *serverWatch) loginLanded(now time.Time) {
	webConnection.Lock()
	c := webConnection.connection
	webConnection.Unlock()
	if c == nil {
		return
	}
	ran, acked := afterLogin(c)
	if !ran {
		return
	}
	log.Printf("rcon: the server answered the login; logged in")
	defer w.storeNextCheck(now)
	Polled.Store(true)
	if PollPlayers() || acked {
		w.answered(now)
	}
}

// heartbeat logs in if there is no live session, sends an empty command,
// and, while the server is down and that goes unanswered, logs in again on
// the same socket. It reports whether the server answered. At most one login
// packet goes out per heartbeat, and none while a login waits for its answer.
func (w *serverWatch) heartbeat() bool {
	if err := Login(); err != nil {
		return false
	}
	_, acked, err := Run("", heartbeatWait, false, "")
	if err == nil && acked {
		return true
	}
	return w.isDown() && Relogin() && PollPlayers()
}

// answered records a round the server answered: the count starts again,
// and the ban list is read after a login and every SyncEvery.
func (w *serverWatch) answered(now time.Time) {
	w.lastAnswered = now
	Fails.Store(0)
	markAnswered()
	if loginPollDue.Swap(false) || time.Since(w.lastBanSync) >= BanSyncEvery {
		w.lastBanSync = time.Now()
		if err := SyncBans(); err != nil {
			log.Printf("rcon: bans: %v", err)
			w.lastBanSync = time.Now().Add(time.Minute - BanSyncEvery) // try again in a minute
		}
	}
}

// storeNextCheck sets NextPoll for the status bar: the next round, or,
// while the server is down, the next heartbeat.
func (w *serverWatch) storeNextCheck(now time.Time) {
	next := now.Add(pollEvery)
	if w.isDown() {
		next = w.lastHeartbeat.Add(heartbeatEvery)
	}
	NextPoll.Store(next.UnixMilli())
}

// database is the player database, set at startup (SetDatabase).
var database *store.Database

// SetDatabase sets the player database this package uses.
func SetDatabase(db *store.Database) { database = db }
