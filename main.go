package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// BattlEye RCON protocol cheat sheet (used throughout this file)
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

var (
	rconAddress    string    // "host:port" of the DayZ/BattlEye RCON endpoint, set in main()
	rconPassword   string    // RCON password, read from the RCON_PASSWORD env var
	globalDatabase *Database // player database (type and helpers live in other files of the package)
)

// environmentOrDefault returns the value of the environment variable `key`,
// or `def` when the variable is unset or empty.
func environmentOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

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

// ---------------------------------------------------------------------------
// One-shot connection: simple, single-threaded, used for short-lived work
// (HTTP requests and CLI calls). Login -> run commands -> close.
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
// dropping the session. Used by the interactive console.
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
// the server acknowledged it within 2 seconds. Used by the console to decide
// whether it needs to reconnect after a failed command.
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
// Player parsing and "kick everybody"
// ---------------------------------------------------------------------------

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
// both oneShotConnection and persistentConnection (and is easy to fake in tests).
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

// ---------------------------------------------------------------------------
// Logging helpers
// ---------------------------------------------------------------------------

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

// logRequests is HTTP middleware that logs each request (method, path+query,
// remote address, X-Forwarded-For if behind a proxy, user agent) before
// passing it on. Health checks and the pages' routine GET polling are skipped
// to keep the log readable.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		quiet := r.Method == http.MethodGet && (r.URL.Path == "/health" || r.URL.Path == "/players" ||
			r.URL.Path == "/bans" || r.URL.Path == "/api/players" || r.URL.Path == "/api/bans")
		if !quiet {
			from := r.RemoteAddr
			if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
				from += " (xff " + xff + ")"
			}
			target := r.URL.Path
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			log.Printf("http %s %s from %s ua=%q", r.Method, target, from, r.UserAgent())
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

// fail writes a JSON error body: {"ok": false, "error": msg} with status `code`.
func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"ok": false, "error": msg})
}

// writeJSON sets the JSON content type, writes the status code, then encodes
// `v` as the response body. (Header must be set before WriteHeader.)
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// runCommand executes a single RCON command for an HTTP caller and writes the
// JSON result.
//   - empty command -> 400
//   - RCON failure  -> 502 (the upstream game server is the problem, not the request)
//   - success       -> 200 with {ok, command, acked, output}
func runCommand(w http.ResponseWriter, command string) {
	command = strings.TrimSpace(command)
	if command == "" {
		fail(w, 400, "no command")
		return
	}
	out, acked, err := sendOneShot("http", command, 2*time.Second)
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "command": command, "acked": acked, "output": out})
}

// handleRcon serves /rcon and accepts the command in three ways, which makes it
// easy to call from simple tools (e.g. a scheduler's "POST to a URI" action):
//
//	GET  /rcon?cmd=players
//	POST /rcon                  body = raw command text
//	POST /rcon (JSON)           body = {"command": "players"}
//
// JSON parsing kicks in when the Content-Type header contains "json".
// Request bodies are capped at 4 KiB.
func handleRcon(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		runCommand(w, r.URL.Query().Get("cmd"))
	case http.MethodPost:
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
		if err != nil {
			fail(w, 400, "bad body")
			return
		}
		cmd := string(raw)
		if strings.Contains(r.Header.Get("Content-Type"), "json") {
			var body struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				fail(w, 400, "bad JSON")
				return
			}
			cmd = body.Command
		}
		runCommand(w, cmd)
	default:
		fail(w, 405, "method not allowed")
	}
}

// handleKickAll serves POST /kickall. The optional JSON body is
//
//	{"reason": "...", "lock": true, "shutdown": true}
//
// An empty body is allowed (all defaults). `reason` defaults to
// "Server restarting". The kickReport is returned under "result".
func handleKickAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, 405, "use POST")
		return
	}
	var body struct {
		Reason   string `json:"reason"`
		Lock     bool   `json:"lock"`
		Shutdown bool   `json:"shutdown"`
	}
	raw, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			fail(w, 400, "bad JSON")
			return
		}
	}
	if body.Reason == "" {
		body.Reason = "Server restarting"
	}
	rep, err := kickAll("http-kickall", body.Reason, body.Lock, body.Shutdown)
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "result": rep})
}

// ---------------------------------------------------------------------------
// CLI output helpers
// ---------------------------------------------------------------------------

// printKickReport prints a human-readable kickall summary. It returns false
// (so the caller can exit non-zero) only when there was an error.
func printKickReport(rep *kickReport, err error) bool {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return false
	}
	if len(rep.Order) == 0 {
		fmt.Println("No players parsed from `players` output:")
		fmt.Println(strings.TrimRight(rep.Raw, "\n"))
		return true
	}
	fmt.Printf("Kicked (highest to lowest): %v\n", rep.Order)
	if len(rep.Unacked) > 0 {
		fmt.Printf("No acknowledgement for: %v\n", rep.Unacked)
	}
	fmt.Printf("Still connected: %v\n", rep.Remaining)
	return true
}

// printResult prints the outcome of a single command. It distinguishes three
// non-error cases so you can tell them apart in the terminal:
//   - output text            -> printed as-is
//   - acked but empty output -> "(ok, no output)"
//   - nothing at all         -> "(no acknowledgement from server)"
//
// Returns false on error so the caller can exit non-zero.
func printResult(out string, acked bool, err error) bool {
	switch {
	case err != nil:
		fmt.Fprintln(os.Stderr, "error:", err)
		return false
	case out != "":
		fmt.Println(strings.TrimRight(out, "\n"))
	case acked:
		fmt.Println("(ok, no output)")
	default:
		fmt.Println("(no acknowledgement from server)")
	}
	return true
}

// ---------------------------------------------------------------------------
// CLI modes
// ---------------------------------------------------------------------------

// runCommandLine handles command-line arguments (anything after the program name):
//
//	kickall [reason...]   kick everyone, print a summary
//	-i                    interactive console
//	<anything else>       joined into one RCON command, run once, printed
//
// Exits with status 1 on failure.
func runCommandLine(args []string) {
	if args[0] == "kickall" {
		rep, err := kickAll("cli", strings.Join(args[1:], " "), false, false)
		if !printKickReport(rep, err) {
			os.Exit(1)
		}
		return
	}
	if args[0] != "-i" {
		out, acked, err := sendOneShot("cli", strings.Join(args, " "), 2*time.Second)
		if !printResult(out, acked, err) {
			os.Exit(1)
		}
		return
	}
	runConsole()
}

// runConsole is the interactive REPL. It logs in once and keeps the session
// alive with the keepalive goroutine, so repeated commands don't pay the
// login round trip. Features:
//   - "exit" / "quit" leaves; blank lines are ignored
//   - "kickall [reason]" works the same as the CLI/HTTP version
//   - automatic reconnect: if the session is dead before a command, or a
//     command fails/gets no ack and the session no longer answers a probe,
//     it logs in again
func runConsole() {
	fmt.Println("Interactive RCON console (logs in once and stays logged in). Type 'exit' to quit.")
	var l *persistentConnection
	defer func() {
		if l != nil {
			l.close()
		}
	}()

	// connect (re)establishes the session, closing any previous one first.
	// Returns false (after printing the error) if login fails.
	connect := func() bool {
		if l != nil {
			l.close()
			l = nil
		}
		nl, err := dialPersistent("console", true)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return false
		}
		l = nl
		return true
	}
	connect()
	sc := bufio.NewScanner(os.Stdin)
	for fmt.Print("rcon> "); sc.Scan(); fmt.Print("rcon> ") {
		line := strings.TrimSpace(sc.Text())
		if line == "exit" || line == "quit" {
			return
		}
		if line == "" {
			continue
		}
		if l == nil || l.isClosed() {
			fmt.Println("(not connected, logging in...)")
			if !connect() {
				continue
			}
		}
		var acked bool
		var err error
		if line == "kickall" || strings.HasPrefix(line, "kickall ") {
			var rep *kickReport
			rep, err = kickAllUsing(l, strings.TrimPrefix(line, "kickall"), false, false)
			printKickReport(rep, err)
			acked = err == nil
		} else {
			var out string
			out, acked, err = l.run(line, 2*time.Second)
			printResult(out, acked, err)
		}
		// A failed or unacknowledged command may mean the session silently
		// died (UDP has no disconnect signal). Probe it and re-login if needed.
		if (err != nil || !acked) && !l.isResponsive() {
			fmt.Println("(session lost, logging in again...)")
			connect()
		}
	}
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

// main wires everything together:
//  1. Reads config from the environment:
//     RCON_PASSWORD (required), RCON_HOST (default 127.0.0.1),
//     RCON_PORT (default 2306), DB_PATH (default /data/players.db),
//     LISTEN_ADDR (default :8080).
//  2. Opens the player database.
//  3. If command-line args were given, runs CLI mode and returns.
//  4. Otherwise runs as a server: clears stale sessions, registers the HTTP
//     routes, starts the background player sync, and serves forever.
func main() {
	rconPassword = os.Getenv("RCON_PASSWORD")
	if rconPassword == "" {
		log.Fatal("RCON_PASSWORD is required")
	}
	rconAddress = net.JoinHostPort(environmentOrDefault("RCON_HOST", "127.0.0.1"), environmentOrDefault("RCON_PORT", "2306"))

	dbPath := environmentOrDefault("DB_PATH", "/data/players.db")
	var err error
	globalDatabase, err = OpenDatabase(dbPath)
	if err != nil {
		log.Fatalf("failed to initialize database: %v", err)
	}

	// CLI mode: `prog kickall ...`, `prog -i`, `prog players`, etc.
	if len(os.Args) > 1 {
		runCommandLine(os.Args[1:])
		return
	}

	// Drop sessions left open by a previous run so restart gaps aren't counted as playtime.
	if _, err := globalDatabase.Exec("DELETE FROM active_sessions"); err != nil {
		log.Printf("failed to clear stale sessions: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("/rcon", handleRcon)                     // run any RCON command
	mux.HandleFunc("/kickall", handleKickAll)               // kick everyone (optionally lock/shutdown)
	mux.HandleFunc("/players", handlePlayersPage)           // players web page
	mux.HandleFunc("/api/players", handlePlayersJSON)       // players JSON list
	mux.HandleFunc("/api/players/", handlePlayerAction)     // per-player actions (trailing slash = subtree)
	mux.HandleFunc("/bans", handleBansPage)                 // bans web page
	mux.HandleFunc("/api/bans", handleBansJSON)             // bans JSON list
	mux.HandleFunc("/api/bans/", handleBanAction)           // per-ban actions

	addr := environmentOrDefault("LISTEN_ADDR", ":8080")
	srv := &http.Server{
		Addr:              addr,
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 5 * time.Second,   // guards against slowloris-style header stalls
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      120 * time.Second, // generous: kickall can take a while
	}
	startBackgroundSync(5 * time.Second) // periodic player/ban polling (defined in another file)
	log.Printf("listening on %s -> RCON %s", addr, rconAddress)
	log.Fatal(srv.ListenAndServe())
}