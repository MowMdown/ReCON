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
//   - three kinds of session:
//       one-shot     dial, log in, run, close (HTTP /rcon, CLI)
//       persistent   stays logged in, background reader, optional keepalive (console)
//       shared       one auto-reconnecting persistent session for the web
//                    layer, the background poller and flows (runShared)
//   - parsing what the server sends back (`players`, `bans`)
//   - multi-command sequences built from those pieces (kickall, reading bans)
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
//	0x02 message  server -> client: unsolicited server message, payload = seq byte.
//	              The client MUST acknowledge it by sending a 0x02 packet with that seq.
//
// An empty command (seq byte only) works as a keepalive; the server drops
// clients that stay silent for roughly 45 seconds.
// ============================================================================

// buildPacket wraps a payload into a complete BattlEye packet:
// 'B' 'E' + CRC32(body) + body, where body = 0xFF + ptype + payload.
// The 4 checksum bytes start as zero placeholders and are filled in once the
// body is known, because the CRC covers only the body, not the header.
func buildPacket(ptype byte, payload []byte) []byte {
	body := append([]byte{0xFF, ptype}, payload...)
	pkt := []byte{'B', 'E', 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(pkt[2:6], crc32.ChecksumIEEE(body))
	return append(pkt, body...)
}

// parsePacket validates a raw datagram and splits it into (type, payload).
// It returns ok=false for anything malformed: too short, missing the 'BE'
// magic, a CRC mismatch, or a body that doesn't start with the 0xFF marker.
// Callers simply ignore packets that fail to parse.
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

// isTimeout reports whether err is a network timeout (e.g. a read deadline
// expiring). Timeouts are an expected, normal way for read loops to end,
// so callers treat them differently from real I/O errors.
func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// errLoginRejected is returned when the server answers the login packet with
// "failed". Usually a wrong password or the client IP not being whitelisted.
var errLoginRejected = errors.New("RCON login rejected (wrong password or IP not allowed)")

// login authenticates on an already-dialed UDP connection.
// UDP is lossy, so it sends the login packet up to 3 times, waiting 1 second
// for a reply after each send. `buf` is a caller-provided scratch buffer for reads.
//
// Outcomes:
//   - login accepted           -> nil
//   - explicit rejection       -> errLoginRejected
//   - real network error       -> that error
//   - silence after 3 attempts -> descriptive error (RCon disabled, wrong port, UDP blocked)
func login(conn net.Conn, buf []byte) error {
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := conn.Write(buildPacket(0, []byte(rconPassword))); err != nil {
			return err
		}
		conn.SetReadDeadline(time.Now().Add(time.Second))
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
	return errors.New("no reply to RCON login after 3 tries: RCon not enabled (no RConPassword), wrong port, or UDP blocked")
}

// runLogged runs an RCON command and logs it to stdout with its source
// ("web", "console", "cli", "http", "http-kickall") and whether the server
// acknowledged it. The web poller's routine `players`/`bans` reads and empty
// keepalive probes are skipped so the log only shows real actions.
// `f` performs the actual command; its result is returned unchanged.
func runLogged(src, command string, f func() (string, bool, error)) (string, bool, error) {
	out, acked, err := f()
	if command != "" && !(src == "web" && (command == "players" || command == "bans")) {
		log.Printf("rcon[%s] > %q (acked=%v err=%v)", src, command, acked, err)
	}
	return out, acked, err
}

// ---------------------------------------------------------------------------
// One-shot connection: simple, single-threaded, used for short-lived work
// (HTTP /rcon and CLI calls). Login -> run commands -> close.
// ---------------------------------------------------------------------------

// oneShotConnection is a logged-in RCON session used synchronously by one
// caller at a time. It does no background reading and no keepalive.
type oneShotConnection struct {
	conn net.Conn // UDP connection to the server
	buf  []byte   // reusable 64 KiB read buffer
	seq  byte     // next command sequence number (wraps naturally at 255)
	src  string   // who is using it ("http", "cli", ...), only used for log lines
}

// dialOneShot opens a UDP connection to rconAddress and logs in.
// On login failure the socket is closed before the error is returned.
func dialOneShot(src string) (*oneShotConnection, error) {
	conn, err := net.Dial("udp", rconAddress)
	if err != nil {
		return nil, err
	}
	s := &oneShotConnection{conn: conn, buf: make([]byte, 65535), src: src}
	if err := login(conn, s.buf); err != nil {
		conn.Close()
		return nil, err
	}
	return s, nil
}

// close releases the underlying UDP socket.
func (s *oneShotConnection) close() { s.conn.Close() }

// run executes a command and logs it (with its source and ack status) via
// runLogged. This is the method that satisfies the commandRunner interface.
func (s *oneShotConnection) run(command string, wait time.Duration) (string, bool, error) {
	return runLogged(s.src, command, func() (string, bool, error) { return s.runRaw(command, wait) })
}

// runRaw sends one command and collects the reply, without logging.
//
// Returns (output, acked, err):
//   - output: the reassembled reply text (may be empty)
//   - acked:  true if the server sent any reply carrying our sequence number,
//     which is how we know the server actually received the command
//   - err:    only for real I/O errors; a timeout just ends the wait
//
// `wait` is the total read deadline for the reply.
func (s *oneShotConnection) runRaw(command string, wait time.Duration) (string, bool, error) {
	seq := s.seq
	s.seq++
	if _, err := s.conn.Write(buildPacket(1, append([]byte{seq}, command...))); err != nil {
		return "", false, err
	}
	s.conn.SetReadDeadline(time.Now().Add(wait))

	// Replies may be split over several packets; collect them keyed by part index.
	parts := map[byte][]byte{}
	acked := false
	for {
		n, err := s.conn.Read(s.buf)
		if err != nil {
			if isTimeout(err) {
				break // deadline hit: return whatever we've gathered
			}
			return "", false, err
		}
		t, p, ok := parsePacket(s.buf[:n])
		if !ok {
			continue
		}
		// Server-pushed message (chat, join/leave, ...): must be acked or the
		// server keeps resending it. Echo back its sequence byte.
		if t == 2 && len(p) > 0 {
			s.conn.Write(buildPacket(2, p[:1]))
			continue
		}
		// A command reply whose first byte matches the seq we sent.
		if t == 1 && len(p) > 0 && p[0] == seq {
			acked = true
			body := p[1:]
			if len(body) >= 3 && body[0] == 0 {
				// Multi-packet reply: body[1] = total parts, body[2] = this part's index.
				parts[body[2]] = append([]byte(nil), body[3:]...)
				if len(parts) == int(body[1]) {
					break // all parts received
				}
			} else {
				// Single-packet reply (or empty ack).
				parts[0] = append([]byte(nil), body...)
				break
			}
		}
	}

	// Stitch the parts back together in index order.
	idx := make([]int, 0, len(parts))
	for k := range parts {
		idx = append(idx, int(k))
	}
	sort.Ints(idx)
	var sb strings.Builder
	for _, i := range idx {
		sb.Write(parts[byte(i)])
	}
	return sb.String(), acked, nil
}

// sendOneShot is the convenience wrapper: dial, log in, run exactly one
// command, close. Used by the HTTP /rcon endpoint and the CLI.
func sendOneShot(src, command string, wait time.Duration) (string, bool, error) {
	s, err := dialOneShot(src)
	if err != nil {
		return "", false, err
	}
	defer s.close()
	return s.run(command, wait)
}

// ---------------------------------------------------------------------------
// Persistent connection: stays logged in, a background goroutine reads all
// traffic, and an optional keepalive goroutine prevents the server from
// dropping the session. Used by the interactive console and (via
// connectShared) by the web layer, poller and flows.
// ---------------------------------------------------------------------------

// keepaliveEvery is how often an empty command is sent to keep the session
// alive. It's a var (not const) so tests can shorten it.
var keepaliveEvery = 30 * time.Second

// persistentConnection is a long-lived RCON session that is safe to use from
// multiple goroutines.
type persistentConnection struct {
	src         string        // label for log lines
	conn        net.Conn      // UDP connection
	writeLock   sync.Mutex    // serialises socket writes and seq allocation
	commandLock sync.Mutex    // ensures only one command is in flight at a time
	seq         byte          // next command sequence number
	replies     chan []byte   // command replies (seq byte + body) handed from reader() to runRaw()
	done        chan struct{} // closed by reader() when the socket dies
	stop        chan struct{} // closed by close() to stop the keepalive goroutine
	once        sync.Once     // makes close() idempotent
	err         error         // the error that killed the reader (valid once done is closed)
}

// write sends one raw packet, holding writeLock so the reader goroutine
// (acking server messages), keepalive and command senders never interleave.
func (l *persistentConnection) write(pkt []byte) error {
	l.writeLock.Lock()
	defer l.writeLock.Unlock()
	_, err := l.conn.Write(pkt)
	return err
}

// dialPersistent connects, logs in, and starts the background goroutines.
// It parses rconAddress manually so it can use DialUDP with an explicit
// UDPAddr. If enableKeepalive is true, an empty-command heartbeat is sent
// every keepaliveEvery.
func dialPersistent(src string, enableKeepalive bool) (*persistentConnection, error) {
	host, portStr, err := net.SplitHostPort(rconAddress)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, err
	}

	conn, err := net.DialUDP("udp", nil, &net.UDPAddr{
		IP:   net.ParseIP(host),
		Port: port,
	})
	if err != nil {
		return nil, err
	}

	buf := make([]byte, 4096)

	if err := login(conn, buf); err != nil {
		conn.Close()
		return nil, err
	}

	// login() left a 1s read deadline on the socket; clear it so the
	// background reader can block indefinitely.
	_ = conn.SetReadDeadline(time.Time{})

	l := &persistentConnection{
		src:     src,
		conn:    conn,
		replies: make(chan []byte, 32),
		done:    make(chan struct{}),
		stop:    make(chan struct{}),
	}

	go l.reader()

	if enableKeepalive {
		go l.keepalive()
	}

	return l, nil
}

// close shuts the session down exactly once: signals the keepalive goroutine
// to stop and closes the socket (which in turn makes reader() exit).
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

// reader is the single goroutine that reads the socket for the lifetime of the
// connection. It:
//   - acknowledges server messages (type 2) immediately,
//   - forwards command replies (type 1) to the replies channel,
//   - drops anything it can't parse.
//
// On a read error it records the error and closes `done`, which wakes up
// anybody waiting in runRaw/keepalive.
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
		case 2:
			// Server message: ack it with the same seq byte.
			l.write(buildPacket(2, p[:1]))
		case 1:
			// Command reply: hand a copy to runRaw. Non-blocking send so a
			// slow consumer can't stall the reader; excess replies are dropped.
			select {
			case l.replies <- append([]byte(nil), p...):
			default:
			}
		}
	}
}

// keepalive sends an empty command (just a seq byte) on a timer so the server
// doesn't time the session out while the console sits idle. The reply, if any,
// is ignored by runRaw's seq filter. Exits when the connection is closed or dies.
func (l *persistentConnection) keepalive() {
	t := time.NewTicker(keepaliveEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			l.writeLock.Lock()
			seq := l.seq
			l.seq++
			l.writeLock.Unlock()
			l.write(buildPacket(1, []byte{seq}))
		case <-l.stop:
			return
		case <-l.done:
			return
		}
	}
}

// isResponsive probes the session with an empty command and reports whether
// the server acknowledged it within 2 seconds. Used to decide whether a
// session needs replacing after a failed command.
func (l *persistentConnection) isResponsive() bool {
	_, acked, err := l.run("", 2*time.Second)
	return err == nil && acked
}

// run executes a command with logging; satisfies the commandRunner interface.
func (l *persistentConnection) run(command string, wait time.Duration) (string, bool, error) {
	return runLogged(l.src, command, func() (string, bool, error) { return l.runRaw(command, wait) })
}

// runRaw sends one command and waits for its (possibly multi-part) reply.
// Same return contract as oneShotConnection.runRaw, but replies arrive via
// the replies channel from reader() instead of reading the socket directly.
//
// Steps:
//  1. Take commandLock so only one command is in flight.
//  2. Drain stale replies (e.g. late answers to an earlier timed-out command
//     or a keepalive) and allocate a seq under writeLock.
//  3. Send the command.
//  4. Collect replies matching our seq until all parts arrive, the timer
//     fires, or the connection dies.
//  5. Reassemble parts in order.
func (l *persistentConnection) runRaw(command string, wait time.Duration) (string, bool, error) {
	l.commandLock.Lock()
	defer l.commandLock.Unlock()
	if l.isClosed() {
		return "", false, l.err
	}

	l.writeLock.Lock()
	for len(l.replies) > 0 {
		<-l.replies // discard stale replies
	}
	seq := l.seq
	l.seq++
	l.writeLock.Unlock()

	if err := l.write(buildPacket(1, append([]byte{seq}, command...))); err != nil {
		return "", false, err
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()
	parts := map[byte][]byte{}
	acked := false
loop:
	for {
		select {
		case p := <-l.replies:
			if p[0] != seq {
				continue // reply to some other command (e.g. keepalive)
			}
			acked = true
			body := p[1:]
			if len(body) >= 3 && body[0] == 0 {
				// Multi-packet reply: [0, total, index, data...]
				parts[body[2]] = append([]byte(nil), body[3:]...)
				if len(parts) == int(body[1]) {
					break loop
				}
			} else {
				// Single-packet reply.
				parts[0] = append([]byte(nil), body...)
				break loop
			}
		case <-timer.C:
			break loop // out of time: return what we have
		case <-l.done:
			return "", false, l.err // connection died while waiting
		}
	}
	idx := make([]int, 0, len(parts))
	for k := range parts {
		idx = append(idx, int(k))
	}
	sort.Ints(idx)
	var sb strings.Builder
	for _, i := range idx {
		sb.Write(parts[byte(i)])
	}
	return sb.String(), acked, nil
}

// ---------------------------------------------------------------------------
// Shared session: the one long-lived connection used by the web layer, the
// background poller and flows.
// ---------------------------------------------------------------------------

// sharedConnection is the single long-lived RCON session used by the web
// layer and the background poller. The embedded Mutex protects the pointer
// (not the commands themselves; persistentConnection serialises those).
// Keepalive is disabled for it (see connectShared) because the 5s poller
// already generates traffic well inside BattlEye's idle timeout.
var sharedConnection = struct {
	sync.Mutex
	connection *persistentConnection
}{}

// connectShared returns the shared session, creating or replacing it as needed:
//   - healthy existing session -> returned as is
//   - existing but dead        -> closed, cleared, and a new one dialed
//   - none                     -> a new one is dialed
//
// The lock is held during dialing, so concurrent callers wait for one login
// rather than each opening their own.
func connectShared() (*persistentConnection, error) {
	sharedConnection.Lock()
	defer sharedConnection.Unlock()
	if sharedConnection.connection != nil && !sharedConnection.connection.isClosed() {
		return sharedConnection.connection, nil
	}
	if sharedConnection.connection != nil {
		sharedConnection.connection.close()
		sharedConnection.connection = nil
	}
	s, err := dialPersistent("web", false)
	if err != nil {
		return nil, err
	}
	sharedConnection.connection = s
	return s, nil
}

// runShared runs a command on the shared session with optional retry/reconnect.
//
//   - allowRetry=false: one attempt. Used for state-changing commands (kick,
//     ban, say, ...) so a slow-but-successful command is never repeated.
//   - allowRetry=true: up to 3 attempts with 1s pauses. Used for read-only
//     commands (players, bans) where repeating is harmless.
//
// After a failed attempt, if there was an error or the session no longer
// answers a probe (isResponsive), the session is discarded so the next
// attempt (or caller) logs in fresh. The pointer comparison under the lock
// avoids discarding a newer session another goroutine already created.
//
// Success means err == nil AND the server acknowledged the command.
func runShared(command string, wait time.Duration, allowRetry bool) (string, bool, error) {
	maxAttempts := 1
	if allowRetry {
		maxAttempts = 3
	}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		s, err := connectShared()
		if err != nil {
			lastErr = err
			time.Sleep(1 * time.Second)
			continue
		}
		out, acked, err := s.run(command, wait)
		if err == nil && acked {
			return out, acked, nil
		}
		if err != nil {
			lastErr = err
		}
		// Drop the session if it errored or has gone silent.
		if err != nil || !s.isResponsive() {
			sharedConnection.Lock()
			if sharedConnection.connection == s {
				s.close()
				sharedConnection.connection = nil
			}
			sharedConnection.Unlock()
		}
		if allowRetry {
			time.Sleep(1 * time.Second)
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("RCON command unacknowledged")
	}
	return "", false, lastErr
}

// sharedRunner adapts the shared RCON session to the commandRunner interface
// that kickAllUsing expects (single attempt per command, no retries).
type sharedRunner struct{}

func (sharedRunner) run(command string, wait time.Duration) (string, bool, error) {
	return runShared(command, wait, false)
}

// ---------------------------------------------------------------------------
// Parsing what the server sends back
// ---------------------------------------------------------------------------

// Player is one row of the BattlEye `players` output, also the JSON shape
// returned by /api/players (offline players come from the DB, see
// Database.OfflinePlayers).
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

// parsePlayers turns the text output of the `players` command into Player
// structs. Each data line looks like:
//
//	12   203.0.113.5:2304   47   a1b2c3...(OK)   Some Name
//
// Fields are whitespace-separated: id, ip:port, ping, guid(status), then the
// name (which may itself contain spaces, so all remaining fields are rejoined).
// The "(OK)"/"(?)" verification suffix is stripped from the GUID. Header lines,
// separators and the footer fail one of the checks and are skipped.
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
		guid := fields[3]
		if idx := strings.IndexByte(guid, '('); idx != -1 {
			guid = guid[:idx]
		}
		name := strings.Join(fields[4:], " ")
		players = append(players, Player{ID: id, IP: host, Port: port, Ping: ping, GUID: guid, Name: name})
	}
	return players
}

// findPlayer parses `players` output and returns the player with the given ID.
// The bool is false if that ID isn't connected.
func findPlayer(out string, id int) (Player, bool) {
	for _, p := range parsePlayers(out) {
		if p.ID == id {
			return p, true
		}
	}
	return Player{}, false
}

// playerRow matches one line of the `players` command output, e.g.
//
//	"12   203.0.113.5:2304   47   a1b2c3(OK)   SomeName"
//
// capturing the numeric player ID at the start of the line. It requires an
// "ip:port" token right after the ID, which filters out the table header,
// separator lines and the "(N players in total)" footer.
var playerRow = regexp.MustCompile(`(?m)^\s*(\d+)\s+\S+:\d+\s`)

// playerIDs extracts every player ID from `players` output and returns them
// sorted from highest to lowest (the order kickAllUsing kicks in).
func playerIDs(out string) []int {
	ids := []int{}
	for _, m := range playerRow.FindAllStringSubmatch(out, -1) {
		if id, err := strconv.Atoi(m[1]); err == nil {
			ids = append(ids, id)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(ids)))
	return ids
}

// guidPattern matches a BattlEye GUID: exactly 32 hex characters. Used both to
// recognise ban rows and to validate the GUID in unban URLs.
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
// Only the GUID section is read; IP bans are ignored. ok is false when the
// "GUID Bans" header wasn't found, so callers never treat an unrecognised
// reply as an empty ban list.
//
// The parser is a tiny state machine: `section` flips to "guid" or "ip" when
// a header line is seen, and only lines in the "guid" section are parsed.
// A data line needs: numeric index, a 32-hex GUID, then "perm" or a number
// of minutes; everything after that is the reason. Lines failing any check
// (column titles, dashes, blank lines) are skipped.
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

// readBanList reads the live GUID ban list from BattlEye (retrying read,
// 4s wait). An unrecognised reply is logged (first 500 chars) and returned
// as an error rather than as an empty list.
func readBanList() ([]Ban, error) {
	out, acked, err := runShared("bans", 4*time.Second, true)
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

// kickReport summarises what a kickall run did. It's JSON-serialisable so the
// HTTP endpoint can return it directly.
type kickReport struct {
	Order         []int  `json:"order"`                    // IDs in the order they were kicked
	Unacked       []int  `json:"unacked"`                  // kick commands the server never acknowledged
	Remaining     []int  `json:"remaining"`                // IDs still connected after the sweep
	Raw           string `json:"raw,omitempty"`            // raw `players` output, only set when nothing could be parsed
	ShutdownSent  bool   `json:"shutdown_sent"`            // whether #shutdown was attempted
	ShutdownAcked bool   `json:"shutdown_acked"`           // whether the server acked #shutdown
	ShutdownError string `json:"shutdown_error,omitempty"` // error text from #shutdown, if any
}

// commandRunner is the minimal interface kickAllUsing needs, so it works with
// oneShotConnection, persistentConnection and sharedRunner (and is easy to
// fake in tests).
type commandRunner interface {
	run(command string, wait time.Duration) (string, bool, error)
}

// kickAll opens a one-shot connection and performs the kick-everyone sequence
// on it. See kickAllUsing for what the flags do.
func kickAll(src, reason string, lock, shutdown bool) (*kickReport, error) {
	s, err := dialOneShot(src)
	if err != nil {
		return nil, err
	}
	defer s.close()
	return kickAllUsing(s, reason, lock, shutdown)
}

// kickAllUsing kicks every connected player using an existing session.
//
// Sequence:
//  1. Sanitise `reason` (CR/LF would let a caller inject extra lines).
//  2. If lock: send "#lock" first so nobody can rejoin mid-sweep.
//  3. Run "players" and parse the IDs.
//  4. "kick <id> <reason>" for each, highest ID first, recording any
//     command that gets no acknowledgement.
//  5. Wait 500ms and re-run "players" to report who is still connected.
//  6. If shutdown: send "#shutdown" and record whether it was acked.
//
// A transport error in steps 2-4 aborts with an error. Failures in the final
// verification or in #shutdown are reported inside the report instead.
func kickAllUsing(s commandRunner, reason string, lock, shutdown bool) (*kickReport, error) {
	reason = strings.NewReplacer("\r", " ", "\n", " ").Replace(strings.TrimSpace(reason))
	if lock {
		if _, _, err := s.run("#lock", 2*time.Second); err != nil {
			return nil, err
		}
	}
	out, _, err := s.run("players", 3*time.Second)
	if err != nil {
		return nil, err
	}
	rep := &kickReport{Order: playerIDs(out), Unacked: []int{}, Remaining: []int{}}
	if len(rep.Order) == 0 {
		rep.Raw = out // help the caller debug why nothing was parsed
	}
	for _, id := range rep.Order {
		cmd := fmt.Sprintf("kick %d %s", id, reason)
		if _, acked, err := s.run(strings.TrimSpace(cmd), 1000*time.Millisecond); err != nil {
			return nil, err
		} else if !acked {
			rep.Unacked = append(rep.Unacked, id)
		}
	}
	if len(rep.Order) > 0 {
		// Give the server a moment to process the kicks, then verify.
		time.Sleep(500 * time.Millisecond)
		if out, _, err := s.run("players", 3*time.Second); err == nil {
			rep.Remaining = playerIDs(out)
		}
	}
	if shutdown {
		rep.ShutdownSent = true
		_, acked, err := s.run("#shutdown", 1500*time.Millisecond)
		rep.ShutdownAcked = acked
		if err != nil {
			rep.ShutdownError = err.Error()
		}
	}
	return rep, nil
}
