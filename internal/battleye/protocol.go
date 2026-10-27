package battleye

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

// BuildPacket wraps a payload into a complete BattlEye packet:
// 'B' 'E' + CRC32(body) + body, where body = 0xFF + ptype + payload.
func BuildPacket(ptype byte, payload []byte) []byte {
	body := append([]byte{0xFF, ptype}, payload...)
	pkt := []byte{'B', 'E', 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(pkt[2:6], crc32.ChecksumIEEE(body))
	return append(pkt, body...)
}

// ParsePacket validates a raw datagram and splits it into (type, payload).
// ok is false for anything malformed (too short, no 'BE', bad CRC, no 0xFF).
func ParsePacket(data []byte) (ptype byte, payload []byte, ok bool) {
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

// ErrNoPassword is returned instead of dialing when no RCON password is set
// (neither RCON_PASSWORD nor RCON Settings).
var ErrNoPassword = errors.New("no RCON password set: set RCON_PASSWORD, or enter one in RCON Settings on the Players page")

// loginWithPassword authenticates on an already-dialed UDP connection. It sends the
// loginWithPassword packet up to `tries` times, waiting `wait` for a reply each time.
// The CLI and console try 3 times (UDP is lossy); the web session sends
// one packet, because every packet a busy server queues becomes another
// "RCon admin logged in" when it catches up.
func loginWithPassword(conn net.Conn, buf []byte, password string, tries int, wait time.Duration) error {
	for attempt := 0; attempt < tries; attempt++ {
		if _, err := conn.Write(BuildPacket(0, []byte(password))); err != nil {
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
			t, p, ok := ParsePacket(buf[:n])
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

// keepaliveEvery is how often an empty command is sent to keep the session
// alive. It's a var (not const) so tests can shorten it.
var keepaliveEvery = 30 * time.Second

// seenWindow is how many recent server-message seq numbers a session
// remembers to drop resends. Seq wraps at 256, so this must stay well below it.
const seenWindow = 32

// Session is a logged-in RCON session that is safe to use from
// multiple goroutines. One goroutine (reader) owns the socket's reads.
type Session struct {
	src         string        // label for log lines ("web", "console", "cli")
	conn        net.Conn      // UDP connection
	password    string        // what it logged in with, for reloginSameSocket()
	writeLock   sync.Mutex    // serialises socket writes and seq allocation
	commandLock sync.Mutex    // ensures only one command is in flight at a time
	seq         byte          // next command sequence number (wraps at 255)
	replies     chan []byte   // command replies (seq byte + body) handed from readServerPackets() to runRawCommand()
	logins      chan byte     // login replies (1 = accepted) handed from readServerPackets() to reloginSameSocket()
	done        chan struct{} // closed by readServerPackets() when the socket dies
	stop        chan struct{} // closed by close() to stop the keepalive goroutine
	once        sync.Once     // makes close() idempotent
	Err         error         // the error that killed the reader (valid once done is closed)
	seen        []byte        // recent server-message seqs; only readServerPackets() touches it
	onMessage   func(string)  // called with each new server message (may be nil)
}

// DialOptions is how Dial logs in and what the session does with
// server messages.
type DialOptions struct {
	Source    string        // label for log lines ("web", "cli")
	Tries     int           // login packets to send before giving up
	ReplyWait time.Duration // how long to wait for each login reply
	Keepalive bool          // send an empty command every keepaliveEvery
	OnMessage func(string)  // called with each new server message (may be nil)
}

// Dial connects to address, logs in with password and starts the
// background reader, as opt says.
func Dial(address, password string, opt DialOptions) (*Session, error) {
	if password == "" {
		return nil, ErrNoPassword
	}
	conn, err := net.Dial("udp", address) // resolves hostnames, e.g. a compose service name
	if err != nil {
		return nil, err
	}
	if err := loginWithPassword(conn, make([]byte, 4096), password, opt.Tries, opt.ReplyWait); err != nil {
		conn.Close()
		return nil, err
	}
	// loginWithPassword() left a 1s read deadline; clear it so readServerPackets() can block.
	_ = conn.SetReadDeadline(time.Time{})

	l := &Session{
		src:       opt.Source,
		conn:      conn,
		password:  password,
		replies:   make(chan []byte, 32),
		logins:    make(chan byte, 4),
		done:      make(chan struct{}),
		stop:      make(chan struct{}),
		onMessage: opt.OnMessage,
	}
	go l.readServerPackets()
	if opt.Keepalive {
		go l.keepSessionAlive()
	}
	return l, nil
}

// write sends one raw packet. Holding writeLock keeps the reader (acking
// server messages), keepalive and command senders from interleaving.
func (l *Session) write(pkt []byte) error {
	l.writeLock.Lock()
	defer l.writeLock.Unlock()
	_, err := l.conn.Write(pkt)
	return err
}

// nextSeq hands out the next command sequence number.
func (l *Session) nextSeq() byte {
	l.writeLock.Lock()
	defer l.writeLock.Unlock()
	s := l.seq
	l.seq++
	return s
}

// close shuts the session down exactly once: stops keepalive and closes the
// socket (which makes readServerPackets() exit).
func (l *Session) Close() {
	l.once.Do(func() {
		close(l.stop)
		l.conn.Close()
	})
}

// isClosed reports whether the reader goroutine has exited (socket dead).
func (l *Session) IsClosed() bool {
	select {
	case <-l.done:
		return true
	default:
		return false
	}
}

// readServerPackets reads the socket for the lifetime of the connection: it acks and
// handles server messages (type 2), forwards command replies (type 1) to
// runRawCommand and login replies (type 0) to relogin. On a read error it records
// the error and closes `done`.
func (l *Session) readServerPackets() {
	buf := make([]byte, 65535)
	for {
		n, err := l.conn.Read(buf)
		if err != nil {
			l.Err = err
			close(l.done)
			return
		}
		t, p, ok := ParsePacket(buf[:n])
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
			select { // a reply to reloginSameSocket()
			case l.logins <- p[0]:
			default:
			}
		case 2:
			// ACK first, every time, even for a resend: a resend means our
			// last ACK was lost, and BattlEye drops clients that don't ACK.
			l.write(BuildPacket(2, p[:1]))
			if l.seenBefore(p[0]) {
				continue // a resend of a message already handled
			}
			l.serverMessage(string(p[1:]))
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
// remembers it if not. Only readServerPackets() calls it, so it needs no lock.
func (l *Session) seenBefore(seq byte) bool {
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

// keepSessionAlive sends an empty command on a timer so the server doesn't drop an
// idle session. runRawCommand's seq filter ignores the replies.
func (l *Session) keepSessionAlive() {
	t := time.NewTicker(keepaliveEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			l.write(BuildPacket(1, []byte{l.nextSeq()}))
		case <-l.stop:
			return
		case <-l.done:
			return
		}
	}
}

// ReloginReplyWait is how long relogin waits for the answer to its one
// login packet.
var ReloginReplyWait = 3 * time.Second

// reloginSameSocket logs in again on this same socket. BattlEye keys admin slots by
// IP and port, so the server keeps the slot it already gave this socket
// instead of opening a new one (and leaving the old one behind until it
// times out). It sends ONE login packet: while the server is restarting, its
// port can take packets without answering and then handle the whole backlog
// at once, so every extra packet becomes another "RCon admin logged in" (see
// rcon.Relogin for the pacing). On success the command sequence starts
// again at 0.
func (l *Session) Relogin() error {
	l.commandLock.Lock() // no command in flight while logging in
	defer l.commandLock.Unlock()
	for len(l.logins) > 0 {
		<-l.logins // late answers to an earlier login
	}
	if err := l.write(BuildPacket(0, []byte(l.password))); err != nil {
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
	case <-time.After(ReloginReplyWait):
		return errors.New("no reply to RCON login")
	case <-l.done:
		return l.Err
	}
}

// isResponsive probes the session with an empty command and reports whether
// the server acknowledged it within 2 seconds.
func (l *Session) IsResponsive() bool {
	_, acked, err := l.Run("", 2*time.Second)
	return err == nil && acked
}

// Run executes a command and logs it to the container log as [cli] (empty
// probes aren't logged). The CLI and its console (-i) use it; the web
// session logs in rcon.Run instead. It satisfies the commandRunner
// interface.
func (l *Session) Run(command string, wait time.Duration) (string, bool, error) {
	out, acked, err := l.RunRaw(command, wait)
	if command != "" {
		LogSentCommand(l.src, "", command, acked, err)
	}
	return out, acked, err
}

// LogSentCommand writes one sent command to the container log, e.g.
//
//	[web] > kick 3 Bye (flow "Ping kicker", acked=true)
//	[cli] > players (acked=true)
//
// origin is the session (webSource or "cli"), by who asked for it.
func LogSentCommand(origin, by, command string, acked bool, err error) {
	detail := fmt.Sprintf("acked=%v", acked)
	if err != nil {
		detail += fmt.Sprintf(" err=%v", err)
	}
	if by != "" {
		detail = by + ", " + detail
	}
	log.Printf("[%s] > %s (%s)", origin, command, detail)
}

// runRawCommand sends one command and waits for its (possibly multi-part) reply.
//
// Returns (output, acked, err):
//   - output: the reassembled reply text (may be empty)
//   - acked:  true if any reply carried our sequence number
//   - err:    only for real I/O errors; running out of time just ends the wait
func (l *Session) RunRaw(command string, wait time.Duration) (string, bool, error) {
	l.commandLock.Lock()
	defer l.commandLock.Unlock()
	if l.IsClosed() {
		return "", false, l.Err
	}
	for len(l.replies) > 0 {
		<-l.replies // discard late answers to an earlier command or keepalive
	}
	seq := l.nextSeq()
	if err := l.write(BuildPacket(1, append([]byte{seq}, command...))); err != nil {
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
			return "", false, l.Err // connection died while waiting
		}
	}
}
