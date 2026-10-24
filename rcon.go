package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"log"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// rcon.go overview
// ----------------------------------------------------------------------------
// Everything that talks to the game server lives here:
//
//   - the BattlEye packet format and login
//   - persistentConnection: a logged-in session with a background reader and
//     an optional keepalive. The CLI and console dial their own; everything in
//     server mode (web, poller, flows) goes through one shared session
//     (runShared). It opens a new socket only when the old one has died;
//     when the server stops answering, the poller logs in again on the same
//     socket (reloginShared), so BattlEye keeps the same admin slot.
//   - server messages: the lines BattlEye pushes to a logged-in session on
//     its own (chat, connects, GUIDs, kicks). The reader ACKs them, parses
//     them into ServerEvents, logs them and publishes them on serverEvents.
//   - parsing what the server sends back (`players`, `bans`)
//   - kickall, built from those pieces
//
// Nothing in this file knows about HTTP or the database.
// ============================================================================

// ============================================================================
// BattlEye RCON protocol cheat sheet
// ----------------------------------------------------------------------------
// Transport: UDP.
// Every packet looks like:
//
//	'B' 'E' | CRC32 (4 bytes, little-endian) | 0xFF | type | payload...
//	\_header_/ \_ checksum of everything after it _/
//
// Packet types:
//	0x00 login    client -> server: payload = password
//	              server -> client: payload = 0x01 (accepted) or 0x00 (rejected)
//	0x01 command  client -> server: payload = seq byte + command text
//	              server -> client: payload = seq byte + response text
//	              Long responses are split: body = 0x00, <total parts>, <part index>, data
//	0x02 message  server -> client: unsolicited server message, payload = seq byte + text.
//	              The client MUST acknowledge it by sending a 0x02 packet with that seq.
//	              If the ACK is lost the server resends the same seq, so a
//	              client can see a message more than once.
//
// An empty command (seq byte only) works as a keepalive; the server drops
// clients that stay silent for roughly 45 seconds.
// ============================================================================

// buildPacket wraps a payload into a complete BattlEye packet:
// 'B' 'E' + CRC32(body) + body, where body = 0xFF + ptype + payload.
func buildPacket(ptype byte, payload []byte) []byte {
	body := append([]byte{0xFF, ptype}, payload...)
	pkt := []byte{'B', 'E', 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(pkt[2:6], crc32.ChecksumIEEE(body))
	return append(pkt, body...)
}

// parsePacket validates a raw datagram and splits it into (type, payload).
// ok is false for anything malformed (too short, no 'BE', bad CRC, no 0xFF).
func parsePacket(data []byte) (ptype byte, payload []byte, ok bool) {
	if len(data) < 8 || data[0] != 'B' || data[1] != 'E' {
		return 0, nil, false
	}
	body := data[6:]
	if binary.LittleEndian.Uint32(data[2:6]) != crc32.ChecksumIEEE(body) || body[0] != 0xFF {
		return 0, nil, false
	}
	return body[1], body[2:], true
}

// isTimeout reports whether err is a network timeout (a read deadline expiring).
func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// errLoginRejected is returned when the server answers the login packet with
// "failed". Usually a wrong password or the client IP not being whitelisted.
var errLoginRejected = errors.New("RCON login rejected (wrong password or IP not allowed)")

// rconTarget is the game server's RCON endpoint ("host:port") and password:
// the docker-compose settings, or what was saved in RCON Settings on the
// Players page (rconSettings, main.go). It changes while ReCON runs when
// RCON Settings connects somewhere new, so it is read under its lock.
var rconTarget struct {
	sync.RWMutex
	address, password string
}

// currentTarget returns the RCON endpoint and password new sessions use.
func currentTarget() (address, password string) {
	rconTarget.RLock()
	defer rconTarget.RUnlock()
	return rconTarget.address, rconTarget.password
}

// setTarget changes the RCON endpoint and password new sessions use.
func setTarget(address, password string) {
	rconTarget.Lock()
	rconTarget.address, rconTarget.password = address, password
	rconTarget.Unlock()
}

// errNoPassword is returned instead of dialing when no RCON password is set
// (neither RCON_PASSWORD nor RCON Settings).
var errNoPassword = errors.New("no RCON password set: set RCON_PASSWORD, or enter one in RCON Settings on the Players page")

// login authenticates on an already-dialed UDP connection. It sends the
// login packet up to `tries` times, waiting `wait` for a reply each time.
// The CLI and console try 3 times (UDP is lossy); the shared session sends
// one packet, because every packet a busy server queues becomes another
// "RCon admin logged in" when it catches up.
func login(conn net.Conn, buf []byte, password string, tries int, wait time.Duration) error {
	for attempt := 0; attempt < tries; attempt++ {
		if _, err := conn.Write(buildPacket(0, []byte(password))); err != nil {
			return err
		}
		conn.SetReadDeadline(time.Now().Add(wait))
		for {
			n, err := conn.Read(buf)
			if err != nil {
				if isTimeout(err) {
					break // no answer within 1s: go to the next attempt
				}
				return err
			}
			t, p, ok := parsePacket(buf[:n])
			if !ok || t != 0 {
				continue // junk or a non-login packet: keep waiting for the real reply
			}
			if len(p) < 1 || p[0] != 1 {
				return errLoginRejected
			}
			return nil
		}
	}
	return errors.New("no reply to RCON login: RCon not enabled (no RConPassword), wrong port, UDP blocked, or the server is still starting")
}

// addPart stores one command-reply body (the payload after the seq byte) and
// reports whether the reply is complete. Long replies arrive as
// 0x00, <total parts>, <part index>, data...
func addPart(parts map[byte][]byte, body []byte) bool {
	if len(body) >= 3 && body[0] == 0 {
		parts[body[2]] = append([]byte(nil), body[3:]...)
		return len(parts) == int(body[1])
	}
	parts[0] = append([]byte(nil), body...)
	return true
}

// joinParts stitches a reply back together in part order.
func joinParts(parts map[byte][]byte) string {
	var sb strings.Builder
	for i := 0; i < 256 && len(parts) > 0; i++ {
		if p, ok := parts[byte(i)]; ok {
			sb.Write(p)
			delete(parts, byte(i))
		}
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// Persistent connection
// ---------------------------------------------------------------------------

// keepaliveEvery is how often an empty command is sent to keep the session
// alive. It's a var (not const) so tests can shorten it.
var keepaliveEvery = 30 * time.Second

// seenWindow is how many recent server-message seq numbers a session
// remembers to drop resends. Seq wraps at 256, so this must stay well below it.
const seenWindow = 32

// persistentConnection is a logged-in RCON session that is safe to use from
// multiple goroutines. One goroutine (reader) owns the socket's reads.
type persistentConnection struct {
	src         string        // label for log lines ("web", "console", "cli")
	conn        net.Conn      // UDP connection
	password    string        // what it logged in with, for relogin()
	writeLock   sync.Mutex    // serialises socket writes and seq allocation
	commandLock sync.Mutex    // ensures only one command is in flight at a time
	seq         byte          // next command sequence number (wraps at 255)
	replies     chan []byte   // command replies (seq byte + body) handed from reader() to runRaw()
	logins      chan byte     // login replies (1 = accepted) handed from reader() to relogin()
	done        chan struct{} // closed by reader() when the socket dies
	stop        chan struct{} // closed by close() to stop the keepalive goroutine
	once        sync.Once     // makes close() idempotent
	err         error         // the error that killed the reader (valid once done is closed)
	seen        []byte        // recent server-message seqs; only reader() touches it
}

// dialPersistent connects to the current target (currentTarget), logs in,
// and starts the background reader (and the keepalive heartbeat when
// enableKeepalive is true). The shared session (sharedSource) sends one
// login and waits 3s; the CLI and console try 3 times, 1s apart.
func dialPersistent(src string, enableKeepalive bool) (*persistentConnection, error) {
	address, password := currentTarget()
	return dialTo(address, password, src, enableKeepalive)
}

// dialTo is dialPersistent for a given endpoint and password.
func dialTo(address, password, src string, enableKeepalive bool) (*persistentConnection, error) {
	if password == "" {
		return nil, errNoPassword
	}
	conn, err := net.Dial("udp", address) // resolves hostnames, e.g. a compose service name
	if err != nil {
		return nil, err
	}
	tries, wait := 3, time.Second
	if src == sharedSource {
		tries, wait = 1, reloginReplyWait
	}
	if err := login(conn, make([]byte, 4096), password, tries, wait); err != nil {
		conn.Close()
		return nil, err
	}
	// login() left a 1s read deadline; clear it so reader() can block.
	_ = conn.SetReadDeadline(time.Time{})

	l := &persistentConnection{
		src:      src,
		conn:     conn,
		password: password,
		replies:  make(chan []byte, 32),
		logins:   make(chan byte, 4),
		done:     make(chan struct{}),
		stop:     make(chan struct{}),
	}
	go l.reader()
	if enableKeepalive {
		go l.keepalive()
	}
	return l, nil
}

// write sends one raw packet. Holding writeLock keeps the reader (acking
// server messages), keepalive and command senders from interleaving.
func (l *persistentConnection) write(pkt []byte) error {
	l.writeLock.Lock()
	defer l.writeLock.Unlock()
	_, err := l.conn.Write(pkt)
	return err
}

// nextSeq hands out the next command sequence number.
func (l *persistentConnection) nextSeq() byte {
	l.writeLock.Lock()
	defer l.writeLock.Unlock()
	s := l.seq
	l.seq++
	return s
}

// close shuts the session down exactly once: stops keepalive and closes the
// socket (which makes reader() exit).
func (l *persistentConnection) close() {
	l.once.Do(func() {
		close(l.stop)
		l.conn.Close()
	})
}

// isClosed reports whether the reader goroutine has exited (socket dead).
func (l *persistentConnection) isClosed() bool {
	select {
	case <-l.done:
		return true
	default:
		return false
	}
}

// reader reads the socket for the lifetime of the connection: it acks and
// handles server messages (type 2), forwards command replies (type 1) to
// runRaw and login replies (type 0) to relogin. On a read error it records
// the error and closes `done`.
func (l *persistentConnection) reader() {
	buf := make([]byte, 65535)
	for {
		n, err := l.conn.Read(buf)
		if err != nil {
			l.err = err
			close(l.done)
			return
		}
		t, p, ok := parsePacket(buf[:n])
		if !ok || len(p) == 0 {
			continue
		}
		switch t {
		case 0:
			if p[0] == 1 {
				// A fresh login (e.g. after a server restart) can start the
				// message numbering again at 0, so forget the seqs seen so far.
				l.seen = l.seen[:0]
			}
			select { // a reply to relogin()
			case l.logins <- p[0]:
			default:
			}
		case 2:
			// ACK first, every time, even for a resend: a resend means our
			// last ACK was lost, and BattlEye drops clients that don't ACK.
			l.write(buildPacket(2, p[:1]))
			if l.seenBefore(p[0]) {
				continue // a resend of a message already handled
			}
			handleServerMessage(l.src, string(p[1:]))
		case 1:
			// Non-blocking so a slow consumer can't stall the reader.
			select {
			case l.replies <- append([]byte(nil), p...):
			default:
			}
		}
	}
}

// seenBefore reports whether a server-message seq was handled recently, and
// remembers it if not. Only reader() calls it, so it needs no lock.
func (l *persistentConnection) seenBefore(seq byte) bool {
	for _, s := range l.seen {
		if s == seq {
			return true
		}
	}
	if len(l.seen) == seenWindow {
		l.seen = l.seen[1:]
	}
	l.seen = append(l.seen, seq)
	return false
}

// keepalive sends an empty command on a timer so the server doesn't drop an
// idle session. runRaw's seq filter ignores the replies.
func (l *persistentConnection) keepalive() {
	t := time.NewTicker(keepaliveEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			l.write(buildPacket(1, []byte{l.nextSeq()}))
		case <-l.stop:
			return
		case <-l.done:
			return
		}
	}
}

// reloginReplyWait is how long relogin waits for the answer to its one
// login packet.
var reloginReplyWait = 3 * time.Second

// relogin logs in again on this same socket. BattlEye keys admin slots by
// IP and port, so the server keeps the slot it already gave this socket
// instead of opening a new one (and leaving the old one behind until it
// times out). It sends ONE login packet: while the server is restarting, its
// port can take packets without answering and then handle the whole backlog
// at once, so every extra packet becomes another "RCon admin logged in" (see
// reloginShared for the pacing). On success the command sequence starts
// again at 0.
func (l *persistentConnection) relogin() error {
	l.commandLock.Lock() // no command in flight while logging in
	defer l.commandLock.Unlock()
	for len(l.logins) > 0 {
		<-l.logins // late answers to an earlier login
	}
	if err := l.write(buildPacket(0, []byte(l.password))); err != nil {
		return err
	}
	select {
	case ok := <-l.logins:
		if ok != 1 {
			return errLoginRejected
		}
		l.writeLock.Lock()
		l.seq = 0
		l.writeLock.Unlock()
		return nil
	case <-time.After(reloginReplyWait):
		return errors.New("no reply to RCON login")
	case <-l.done:
		return l.err
	}
}

// isResponsive probes the session with an empty command and reports whether
// the server acknowledged it within 2 seconds.
func (l *persistentConnection) isResponsive() bool {
	_, acked, err := l.run("", 2*time.Second)
	return err == nil && acked
}

// run executes a command and logs it with its source and ack status (empty
// probes aren't logged). The CLI and console use it; the shared session logs
// in runShared instead, only when asked to. It satisfies the commandRunner
// interface.
func (l *persistentConnection) run(command string, wait time.Duration) (string, bool, error) {
	out, acked, err := l.runRaw(command, wait)
	if command != "" {
		log.Printf("rcon[%s] > %q (acked=%v err=%v)", l.src, command, acked, err)
	}
	return out, acked, err
}

// runRaw sends one command and waits for its (possibly multi-part) reply.
//
// Returns (output, acked, err):
//   - output: the reassembled reply text (may be empty)
//   - acked:  true if any reply carried our sequence number
//   - err:    only for real I/O errors; running out of time just ends the wait
func (l *persistentConnection) runRaw(command string, wait time.Duration) (string, bool, error) {
	l.commandLock.Lock()
	defer l.commandLock.Unlock()
	if l.isClosed() {
		return "", false, l.err
	}
	for len(l.replies) > 0 {
		<-l.replies // discard late answers to an earlier command or keepalive
	}
	seq := l.nextSeq()
	if err := l.write(buildPacket(1, append([]byte{seq}, command...))); err != nil {
		return "", false, err
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()
	parts := map[byte][]byte{}
	acked := false
	for {
		select {
		case p := <-l.replies:
			if p[0] != seq {
				continue // reply to some other command (e.g. keepalive)
			}
			acked = true
			if addPart(parts, p[1:]) {
				return joinParts(parts), true, nil
			}
		case <-timer.C:
			return joinParts(parts), acked, nil // out of time: return what we have
		case <-l.done:
			return "", false, l.err // connection died while waiting
		}
	}
}

// ---------------------------------------------------------------------------
// Shared session (server mode: web layer, poller, flows)
// ---------------------------------------------------------------------------

// sharedConnection holds the one long-lived session. The Mutex protects the
// pointer; persistentConnection serialises the commands themselves.
// Keepalive is off because the 5s poller already keeps the session busy.
// Being permanently logged in is also what lets it receive server messages.
var sharedConnection = struct {
	sync.Mutex
	connection *persistentConnection
	nextDial   time.Time     // no new socket before this, after a failed login
	dialWait   time.Duration // the wait used after the last failed login
	lastErr    error         // returned while waiting for nextDial
}{}

// nextDialAt is when connectShared may next open a new socket after a failed
// login, or zero when it isn't waiting (a session is open, or it may dial now).
func nextDialAt() time.Time {
	sharedConnection.Lock()
	defer sharedConnection.Unlock()
	if c := sharedConnection.connection; c != nil && !c.isClosed() {
		return time.Time{}
	}
	return sharedConnection.nextDial
}

// sharedSource is the shared session's label in log lines.
const sharedSource = "web"

// connectShared returns the shared session, dialing a new one if there is
// none or the current one has died. The lock is held while dialing, so
// concurrent callers wait for one login instead of each opening their own.
//
// Every new login sends #unlock. The shared session logs in when ReCON starts
// and after the session drops, most often because the server restarted, so a
// restart flow's #lock never outlives the restart. (A lock set some other way
// is undone if the session reconnects while it's in place; that's accepted.)
// The CLI and console dial their own sessions and don't unlock.
func connectShared() (*persistentConnection, error) {
	sharedConnection.Lock()
	defer sharedConnection.Unlock()
	if c := sharedConnection.connection; c != nil && !c.isClosed() {
		return c, nil
	}
	if c := sharedConnection.connection; c != nil {
		log.Printf("rcon: connection dropped (socket closed: %v), logging in again", c.err)
		c.close()
		sharedConnection.connection = nil
	}
	// Every call would otherwise dial (the poller, every page refresh), and
	// each new socket leaves a login queued at a server that isn't answering:
	// after a failed login, wait as reloginShared does before dialing again.
	if time.Now().Before(sharedConnection.nextDial) {
		return nil, sharedConnection.lastErr
	}
	s, err := dialPersistent(sharedSource, false)
	if err != nil {
		sharedConnection.dialWait = min(max(2*sharedConnection.dialWait, reloginFirstWait), reloginMaxWait)
		sharedConnection.nextDial = time.Now().Add(sharedConnection.dialWait)
		sharedConnection.lastErr = fmt.Errorf("%w (next login in %s)", err, sharedConnection.dialWait)
		return nil, err
	}
	sharedConnection.nextDial, sharedConnection.dialWait = time.Time{}, 0
	_, acked, uerr := s.runRaw("#unlock", 2*time.Second)
	log.Printf("rcon: logged in, sent #unlock (acked=%v err=%v)", acked, uerr)
	bansStale.Store(true) // the server may have restarted: re-read its ban list (db.go)
	sharedConnection.connection = s
	return s, nil
}

// reconnectShared logs the shared session in to address with password on a
// new socket and, only if the server accepts it, makes that the shared
// session and the target for later logins, closing the old session. This is
// RCON Settings' Connect button: a wrong address or password changes
// nothing. Like every new login it sends #unlock and re-reads the ban list.
func reconnectShared(address, password string) error {
	sharedConnection.Lock()
	defer sharedConnection.Unlock()
	s, err := dialTo(address, password, sharedSource, false)
	if err != nil {
		return err
	}
	setTarget(address, password)
	if old := sharedConnection.connection; old != nil {
		old.close()
	}
	sharedConnection.connection = s
	sharedConnection.nextDial, sharedConnection.dialWait = time.Time{}, 0
	_, acked, uerr := s.runRaw("#unlock", 2*time.Second)
	log.Printf("rcon: connected to %s from RCON Settings, sent #unlock (acked=%v err=%v)", address, acked, uerr)
	bansStale.Store(true)
	return nil
}

// Pacing for reloginShared while the server isn't answering. A login that
// gets no reply waits reloginFirstWait before the next try, doubling up to
// reloginMaxWait, so a long restart leaves only a few login packets queued
// at the server. After a login is accepted, no new one is sent for
// reloginSettle: a server that is still starting up can accept a login
// before it answers `players`, and logging in again then would only add
// another "logged in" line and another #unlock.
var (
	reloginFirstWait = 10 * time.Second
	reloginMaxWait   = 30 * time.Second
	reloginSettle    = 60 * time.Second
)

// reloginPace is when reloginShared may next send a login, and the wait to
// use after the next unanswered one. Only the poller goroutine touches it.
var reloginPace struct {
	next time.Time
	wait time.Duration
}

// resetReloginPace is called by the poller when the server answers again, so
// the next outage starts with an immediate login and the shortest wait.
func resetReloginPace() {
	reloginPace.next, reloginPace.wait = time.Time{}, 0
}

// reloginShared is called by the poller when the server has missed 3 checks
// in a row (it may have restarted and forgotten us). It logs in again on the
// same socket, so the server keeps our admin slot, then sends #unlock as a
// new login does. Calls are paced (see reloginFirstWait): one between
// allowed times does nothing. If there is no live session, the next command
// opens one.
func reloginShared() {
	now := time.Now()
	if now.Before(reloginPace.next) {
		return
	}
	sharedConnection.Lock()
	c := sharedConnection.connection
	sharedConnection.Unlock()
	if c == nil || c.isClosed() {
		return
	}
	if err := c.relogin(); err != nil {
		reloginPace.wait = min(max(2*reloginPace.wait, reloginFirstWait), reloginMaxWait)
		reloginPace.next = time.Now().Add(reloginPace.wait)
		log.Printf("rcon: no reply from the server; logging in again failed (%v), next try in %s", err, reloginPace.wait)
		return
	}
	reloginPace.next, reloginPace.wait = time.Now().Add(reloginSettle), 0
	_, acked, uerr := c.runRaw("#unlock", 2*time.Second)
	log.Printf("rcon: no reply from the server; logged in again on the same connection, sent #unlock (acked=%v err=%v)", acked, uerr)
	bansStale.Store(true)
}

// runShared runs a command on the shared session.
//
//   - allowRetry=false: one attempt. Used for state-changing commands (kick,
//     ban, say, ...) so a slow-but-successful command is never repeated.
//   - allowRetry=true: up to 3 attempts with 1s pauses, for read-only
//     commands (players, bans) where repeating is harmless.
//   - logAs: the label for the "rcon[<logAs>] > ..." log line written for
//     each attempt: "web" for POST actions from the pages and API, the flow's
//     name for flow steps, or "" for no line at all (page refreshes, the
//     poller and other routine reads).
//
// err is only set for a connection/transport failure. A command the server
// simply didn't acknowledge returns acked=false with err=nil, so callers can
// tell "server never got it" from "server didn't answer" (e.g. #shutdown).
//
// A missed reply never drops the session here: retries reuse it, and only
// the poller decides the server has stopped answering (see reloginShared).
// A socket that has died is replaced by connectShared on the next call.
func runShared(command string, wait time.Duration, allowRetry bool, logAs string) (string, bool, error) {
	maxAttempts := 1
	if allowRetry {
		maxAttempts = 3
	}
	// Commands typed in the console page also go to its log, once per call
	// however many attempts it takes. Commands from flows and the other
	// pages don't: the Flows page and the container log already show them.
	if logAs == consoleSource {
		serverEvents.publish(ServerEvent{Time: time.Now(), Kind: eventSent, Text: "> " + command, Source: logAs, PlayerID: -1})
	}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		more := attempt+1 < maxAttempts // only pause when another attempt follows
		s, err := connectShared()
		if err != nil {
			lastErr = err
			if more {
				time.Sleep(1 * time.Second)
			}
			continue
		}
		out, acked, err := s.runRaw(command, wait)
		if logAs != "" {
			log.Printf("rcon[%s] > %q (acked=%v err=%v)", logAs, command, acked, err)
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

// sharedRunner adapts runShared to the commandRunner interface (single
// attempt per command) for kickAll. It doesn't log the individual `players`
// reads and kicks; the caller logs one kickReport.summary line instead.
type sharedRunner struct{}

func (sharedRunner) run(command string, wait time.Duration) (string, bool, error) {
	return runShared(command, wait, false, "")
}

// ---------------------------------------------------------------------------
// Server messages (packet type 0x02)
// ---------------------------------------------------------------------------
//
// While a session is logged in, BattlEye pushes lines to it without being
// asked: chat, players connecting and disconnecting, verified GUIDs, kicks,
// other RCon admins logging in. reader() ACKs each one and drops resends,
// then handleServerMessage turns it into a ServerEvent, logs it as
//
//	rcon[web] < (Global) SomeName: hello
//
// and publishes it on serverEvents. Anything can subscribe (an HTTP feed,
// flows reacting to chat, ...). Publishing never blocks the reader: a
// subscriber that falls behind misses events instead.
//
// Messages sent while the server isn't talking to us (a restart, before
// reloginShared gets in again) are lost; BattlEye keeps no backlog.

// consoleSource is the runShared logAs label for commands typed in the
// console page; only those are echoed into its log.
const consoleSource = "console"

// Server event kinds.
const (
	eventConnect    = "connect"    // Player #3 Name (1.2.3.4:2304) connected
	eventGUID       = "guid"       // Verified GUID (...) of player #3 Name / Player #3 Name - BE GUID: ...
	eventDisconnect = "disconnect" // Player #3 Name disconnected
	eventKick       = "kick"       // Player #3 Name (guid) has been kicked by BattlEye: ...
	eventChat       = "chat"       // (Global) Name: text
	eventAdmin      = "admin"      // RCon admin #0 (1.2.3.4:5678) logged in
	eventOther      = "other"      // anything not recognised; Text still has the line

	// Not from BattlEye: lines ReCON adds so the console shows both sides.
	eventSent  = "sent"  // > say -1 hello   (a command typed in the console page)
	eventReply = "reply" // the server's answer to a console command
	eventError = "error" // a console command that failed or got no answer
)

// ServerEvent is one parsed server message. Fields a kind doesn't have stay
// empty; PlayerID is -1 when the line names no player number (chat, admin,
// other).
type ServerEvent struct {
	Seq      int64     `json:"seq"` // set by serverEvents.publish: 1, 2, 3, ... for this run of ReCON
	Time     time.Time `json:"time"`
	Kind     string    `json:"kind"`
	Text     string    `json:"text"` // the line exactly as BattlEye sent it
	PlayerID int       `json:"playerId"`
	Name     string    `json:"name,omitempty"`
	IP       string    `json:"ip,omitempty"`
	Port     string    `json:"port,omitempty"`
	GUID     string    `json:"guid,omitempty"`    // lower-cased
	Channel  string    `json:"channel,omitempty"` // chat: Global, Side, Direct, Vehicle, ...
	Message  string    `json:"message,omitempty"` // chat text, or the kick reason
	Source   string    `json:"source,omitempty"`  // sent: who asked ("web", "console", a flow's name)
}

var (
	connectRe    = regexp.MustCompile(`^Player #(\d+) (.+) \(([^()]+):(\d+)\) connected$`)
	verifiedRe   = regexp.MustCompile(`^Verified GUID \(([0-9a-fA-F]{32})\) of player #(\d+) (.+)$`)
	beGUIDRe     = regexp.MustCompile(`^Player #(\d+) (.+) - (?:BE )?GUID: ([0-9a-fA-F]{32})$`)
	disconnectRe = regexp.MustCompile(`^Player #(\d+) (.+) disconnected$`)
	kickRe       = regexp.MustCompile(`^Player #(\d+) (.+) \(([0-9a-fA-F]{32}|-)\) has been kicked by BattlEye: (.*)$`)
	adminRe      = regexp.MustCompile(`^RCon admin #(\d+) \(([^()]+):(\d+)\) logged in$`)
	chatRe       = regexp.MustCompile(`^\(([A-Za-z]+)\) (.+?): (.*)$`)
)

// parseServerMessage turns one server message into a ServerEvent. Lines it
// doesn't recognise come back as eventOther with Text set, so nothing is lost
// if BattlEye's wording differs from what's expected here.
func parseServerMessage(text string, at time.Time) ServerEvent {
	ev := ServerEvent{Time: at, Kind: eventOther, Text: text, PlayerID: -1}
	id := func(s string) int { n, _ := strconv.Atoi(s); return n }
	// Kick before disconnect/connect: its name part can contain anything.
	if m := kickRe.FindStringSubmatch(text); m != nil {
		ev.Kind, ev.PlayerID, ev.Name, ev.Message = eventKick, id(m[1]), m[2], m[4]
		if m[3] != "-" {
			ev.GUID = strings.ToLower(m[3])
		}
	} else if m := connectRe.FindStringSubmatch(text); m != nil {
		ev.Kind, ev.PlayerID, ev.Name, ev.IP, ev.Port = eventConnect, id(m[1]), m[2], m[3], m[4]
	} else if m := verifiedRe.FindStringSubmatch(text); m != nil {
		ev.Kind, ev.GUID, ev.PlayerID, ev.Name = eventGUID, strings.ToLower(m[1]), id(m[2]), m[3]
	} else if m := beGUIDRe.FindStringSubmatch(text); m != nil {
		ev.Kind, ev.PlayerID, ev.Name, ev.GUID = eventGUID, id(m[1]), m[2], strings.ToLower(m[3])
	} else if m := disconnectRe.FindStringSubmatch(text); m != nil {
		ev.Kind, ev.PlayerID, ev.Name = eventDisconnect, id(m[1]), m[2]
	} else if m := adminRe.FindStringSubmatch(text); m != nil {
		ev.Kind, ev.IP, ev.Port = eventAdmin, m[2], m[3]
	} else if m := chatRe.FindStringSubmatch(text); m != nil {
		ev.Kind, ev.Channel, ev.Name, ev.Message = eventChat, m[1], m[2], m[3]
	}
	return ev
}

// handleServerMessage is called by reader() once per new server message.
func handleServerMessage(src, text string) {
	text = strings.TrimRight(text, "\r\n ")
	if text == "" {
		return
	}
	log.Printf("rcon[%s] < %s", src, text)
	serverEvents.publish(parseServerMessage(text, time.Now()))
}

// recentEventsKept is how many events serverEvents keeps for late joiners
// (e.g. a page that opens and wants the last few chat lines).
const recentEventsKept = 200

// eventHub fans server events out to subscribers and keeps the most recent
// ones. Safe for concurrent use.
type eventHub struct {
	mu     sync.Mutex
	seq    int64 // Seq of the last published event
	recent []ServerEvent
	subs   map[chan ServerEvent]struct{}
}

// serverEvents receives every server message from every session in this
// process (in server mode, only the shared session exists).
var serverEvents = &eventHub{subs: map[chan ServerEvent]struct{}{}}

// publish numbers an event, records it and hands it to every subscriber
// that has room.
func (h *eventHub) publish(ev ServerEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	ev.Seq = h.seq
	h.recent = append(h.recent, ev)
	// Trim only once twice the limit has built up, so the copy happens once
	// per recentEventsKept events instead of on every one.
	if len(h.recent) >= 2*recentEventsKept {
		h.recent = append([]ServerEvent(nil), h.recent[len(h.recent)-recentEventsKept:]...)
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
func (h *eventHub) subscribe(buffer int) (<-chan ServerEvent, func()) {
	ch := make(chan ServerEvent, buffer)
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

// recentEvents returns a copy of the kept events, oldest first.
func (h *eventHub) recentEvents() []ServerEvent {
	return h.after(0)
}

// after returns the kept events with Seq greater than seq, oldest first, and
// the Seq of the newest event (to pass next time). A page polls with the last
// value it got; 0 means "everything kept". A seq from before a ReCON restart
// is larger than anything now, so the page gets the whole buffer again.
func (h *eventHub) after(seq int64) []ServerEvent {
	events, _ := h.afterWithLast(seq)
	return events
}

func (h *eventHub) afterWithLast(seq int64) ([]ServerEvent, int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if seq > h.seq {
		seq = 0 // ReCON restarted since the caller last asked
	}
	out := []ServerEvent{}
	kept := h.recent
	if len(kept) > recentEventsKept {
		kept = kept[len(kept)-recentEventsKept:]
	}
	for _, ev := range kept {
		if ev.Seq > seq {
			out = append(out, ev)
		}
	}
	return out, h.seq
}

// ---------------------------------------------------------------------------
// Parsing what the server sends back
// ---------------------------------------------------------------------------

// Player is one row of the BattlEye `players` output, also the JSON shape
// returned by /api/players (offline players come from the DB).
type Player struct {
	ID                int    `json:"id"`
	IP                string `json:"ip"`
	Port              string `json:"port"`
	Ping              int    `json:"ping"`
	GUID              string `json:"guid"`
	Name              string `json:"name"`
	Offline           bool   `json:"offline"`
	TotalPlaytimeSecs int64  `json:"totalPlaytimeSeconds"`
	LastSeenUnix      int64  `json:"lastSeenUnix"` // offline players: when they were last seen (Unix seconds)
	Role              int    `json:"role"`         // 0 Player, 1 Mod, 2 Admin (from the database; filled in by the pages' API)
	// Online players: the newest line they wrote in chat this session (not messages sent
	// to them), its channel and when (Unix seconds). Filled in by /api/players.
	LastMessage        string `json:"lastMessage,omitempty"`
	LastMessageChannel string `json:"lastMessageChannel,omitempty"`
	LastMessageUnix    int64  `json:"lastMessageUnix,omitempty"`
}

// Ban is one entry of the GUID section of the BattlEye `bans` output, also the
// JSON shape returned by /api/bans.
type Ban struct {
	Index   int    `json:"index"`   // position in the BattlEye list (shifts after each removal!)
	GUID    string `json:"guid"`    // banned GUID, lower-cased
	Name    string `json:"name"`    // last-known player name, filled from the DB (BattlEye doesn't store it)
	Minutes string `json:"minutes"` // minutes left as text, or "perm"
	Perm    bool   `json:"perm"`    // true for permanent bans
	Reason  string `json:"reason"`
}

// cleanReason trims whitespace and replaces CR/LF with spaces so user-supplied
// text can't break out of a single RCON command line.
func cleanReason(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(strings.TrimSpace(s))
}

// parsePlayers turns `players` output into Player structs. Each data line is
//
//	12   203.0.113.5:2304   47   a1b2c3...(OK)   Some Name
//
// i.e. id, ip:port, ping, guid(status), then the name (which may contain
// spaces). Header, separator and footer lines fail a check and are skipped.
func parsePlayers(out string) []Player {
	players := []Player{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		id, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		host, port, ok := strings.Cut(fields[1], ":")
		if !ok {
			continue
		}
		ping, err := strconv.Atoi(fields[2])
		if err != nil {
			continue
		}
		guid, _, _ := strings.Cut(fields[3], "(") // strip the (OK)/(?) suffix
		if !guidPattern.MatchString(guid) {
			guid = "" // "-": not verified yet; picked up on a later poll
		}
		name := strings.Join(fields[4:], " ")
		players = append(players, Player{ID: id, IP: host, Port: port, Ping: ping, GUID: guid, Name: name})
	}
	return players
}

// findPlayer returns the connected player with the given ID, if any.
func findPlayer(out string, id int) (Player, bool) {
	for _, p := range parsePlayers(out) {
		if p.ID == id {
			return p, true
		}
	}
	return Player{}, false
}

// bareName drops the "(Lobby)" tag BattlEye adds while a player is loading.
func bareName(name string) string {
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(name), lobbySuffix))
}

// onlineByName, onlineByID and onlineByGUID look a player up in the poller's
// latest `players` list (flows.go), so they never send a command of their own.
func onlineByName(name string) (Player, bool) {
	return onlineWhere(func(p Player) bool { return bareName(p.Name) == name })
}

// onlineByUniqueName finds who wrote a chat line for the chat commands and
// Player message flows: the one online player with exactly that name. ok is
// false when nobody, or more than one player, has it.
func onlineByUniqueName(name string) (Player, bool) {
	latest.Lock()
	defer latest.Unlock()
	var found Player
	n := 0
	for _, p := range latest.players {
		if bareName(p.Name) == name {
			found, n = p, n+1
		}
	}
	return found, n == 1
}

func onlineByID(id int) (Player, bool) {
	return onlineWhere(func(p Player) bool { return p.ID == id })
}

func onlineByGUID(guid string) (Player, bool) {
	return onlineWhere(func(p Player) bool { return strings.EqualFold(p.GUID, guid) })
}

func onlineWhere(match func(Player) bool) (Player, bool) {
	latest.Lock()
	defer latest.Unlock()
	for _, p := range latest.players {
		if match(p) {
			return p, true
		}
	}
	return Player{}, false
}

// playerIDs returns every player ID in `players` output, highest first (the
// order kickall kicks in).
func playerIDs(out string) []int {
	ids := []int{}
	for _, p := range parsePlayers(out) {
		ids = append(ids, p.ID)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(ids)))
	return ids
}

// guidPattern matches a BattlEye GUID: exactly 32 hex characters.
var guidPattern = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)

// parseBans parses BattlEye `bans` output, e.g.
//
//	GUID Bans:
//	[#] [GUID] [Minutes left] [Reason]
//	----------------------------------------
//	0  49aacf68461f67163d0a2ecb6a4cee94 perm testing
//
//	IP Bans:
//	...
//
// Only the GUID section is read. ok is false when the "GUID Bans" header
// wasn't found, so an unrecognised reply is never mistaken for an empty list.
func parseBans(out string) (bans []Ban, ok bool) {
	bans = []Ban{}
	section := ""
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		low := strings.ToLower(line)
		switch {
		case strings.HasPrefix(low, "guid bans"):
			section, ok = "guid", true
			continue
		case strings.HasPrefix(low, "ip bans"):
			section = "ip"
			continue
		}
		if section != "guid" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		idx, err := strconv.Atoi(fields[0])
		if err != nil || !guidPattern.MatchString(fields[1]) {
			continue
		}
		perm := strings.EqualFold(fields[2], "perm")
		if !perm {
			if _, err := strconv.Atoi(fields[2]); err != nil {
				continue
			}
		}
		bans = append(bans, Ban{
			Index:   idx,
			GUID:    strings.ToLower(fields[1]),
			Minutes: strings.ToLower(fields[2]),
			Perm:    perm,
			Reason:  strings.Join(fields[3:], " "),
		})
	}
	return bans, ok
}

// readBanList reads the live GUID ban list from BattlEye (retrying read).
// An unrecognised reply is logged and returned as an error, not an empty list.
func readBanList() ([]Ban, error) {
	out, acked, err := runShared("bans", 4*time.Second, true, "")
	if err != nil {
		return nil, err
	}
	if !acked {
		return nil, fmt.Errorf("RCON unavailable")
	}
	bans, ok := parseBans(out)
	if !ok {
		if len(out) > 500 {
			out = out[:500]
		}
		log.Printf("bans: unrecognised output: %q", out)
		return nil, fmt.Errorf("could not parse bans output")
	}
	return bans, nil
}

// ---------------------------------------------------------------------------
// Kick everybody
// ---------------------------------------------------------------------------

// kickReport summarises what a kickall run did (returned as JSON by /kickall).
type kickReport struct {
	Order     []int  `json:"order"`         // IDs in the order they were kicked
	Unacked   []int  `json:"unacked"`       // kick commands the server never acknowledged
	Remaining []int  `json:"remaining"`     // IDs still connected after the sweep
	Raw       string `json:"raw,omitempty"` // raw `players` output, only set when nothing could be parsed
}

// summary is the one log line for a kickall, e.g.
//
//	kickall "Server restarting": kicked [3 0], no ack for [], still connected []
func (rep *kickReport) summary(reason string) string {
	return fmt.Sprintf("kickall %q: kicked %v, no ack for %v, still connected %v", cleanReason(reason), rep.Order, rep.Unacked, rep.Remaining)
}

// commandRunner is what kickAll needs: a persistentConnection (CLI,
// console) or sharedRunner (server mode).
type commandRunner interface {
	run(command string, wait time.Duration) (string, bool, error)
}

// kickAll kicks every connected player and nothing else (#lock,
// #shutdown and the like are separate commands, sent before or after):
//  1. "players", then "kick <id> <reason>" for each, highest ID first
//  2. wait 500ms and re-read "players" to report who is still connected
//
// A transport error in step 1 aborts with an error. Unacknowledged kicks and
// a failed verification are reported in the report.
func kickAll(s commandRunner, reason string) (*kickReport, error) {
	reason = cleanReason(reason)
	out, _, err := s.run("players", 3*time.Second)
	if err != nil {
		return nil, err
	}
	rep := &kickReport{Order: playerIDs(out), Unacked: []int{}, Remaining: []int{}}
	if len(rep.Order) == 0 {
		rep.Raw = out // help the caller debug why nothing was parsed
	}
	for _, id := range rep.Order {
		cmd := strings.TrimSpace(fmt.Sprintf("kick %d %s", id, reason))
		if _, acked, err := s.run(cmd, time.Second); err != nil {
			return nil, err
		} else if !acked {
			rep.Unacked = append(rep.Unacked, id)
		}
	}
	if len(rep.Order) > 0 {
		time.Sleep(500 * time.Millisecond) // let the server process the kicks
		if out, _, err := s.run("players", 3*time.Second); err == nil {
			rep.Remaining = playerIDs(out)
		}
	}
	return rep, nil
}
