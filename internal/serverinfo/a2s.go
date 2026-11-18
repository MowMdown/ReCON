package serverinfo

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"
)

// The Steam server query (A2S), which DayZ answers on its query port (27016
// by default): A2S_INFO for the name, map, players and keywords, A2S_RULES
// for the mod list. https://developer.valvesoftware.com/wiki/Server_queries

var (
	a2sHeader     = []byte{0xFF, 0xFF, 0xFF, 0xFF}
	a2sInfoReq    = append(append([]byte{}, a2sHeader...), append([]byte("TSource Engine Query"), 0)...)
	errA2SShort   = errors.New("short answer")
	errA2SUnknown = errors.New("unexpected answer")
)

// a2sConn is one UDP socket to the query port.
type a2sConn struct {
	c       *net.UDPConn
	timeout time.Duration
}

func dialA2S(addr string, timeout time.Duration) (*a2sConn, error) {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	c, err := net.DialUDP("udp", nil, ua)
	if err != nil {
		return nil, err
	}
	return &a2sConn{c: c, timeout: timeout}, nil
}

func (a *a2sConn) close() { a.c.Close() }

// read reads one answer, putting split answers back together.
func (a *a2sConn) read() ([]byte, error) {
	buf := make([]byte, 65535)
	a.c.SetReadDeadline(time.Now().Add(a.timeout))
	n, err := a.c.Read(buf)
	if err != nil {
		return nil, err
	}
	p := append([]byte{}, buf[:n]...)
	if len(p) < 5 {
		return nil, errA2SShort
	}
	if !bytes.Equal(p[:4], []byte{0xFE, 0xFF, 0xFF, 0xFF}) {
		if !bytes.Equal(p[:4], a2sHeader) {
			return nil, errA2SUnknown
		}
		return p[4:], nil
	}
	// Split: id (4), total (1), number (1), size (2), then the part.
	parts := map[int][]byte{}
	total := -1
	for {
		if len(p) < 12 {
			return nil, errA2SShort
		}
		if int32(binary.LittleEndian.Uint32(p[4:8])) < 0 {
			return nil, errors.New("compressed answers aren't supported")
		}
		total = int(p[8])
		parts[int(p[9])] = p[12:]
		if len(parts) >= total {
			break
		}
		a.c.SetReadDeadline(time.Now().Add(a.timeout))
		n, err := a.c.Read(buf)
		if err != nil {
			return nil, err
		}
		p = append([]byte{}, buf[:n]...)
	}
	var all []byte
	for i := 0; i < total; i++ {
		part, ok := parts[i]
		if !ok {
			return nil, errA2SShort
		}
		all = append(all, part...)
	}
	if len(all) < 5 || !bytes.Equal(all[:4], a2sHeader) {
		return nil, errA2SUnknown
	}
	return all[4:], nil
}

// ask sends req and returns the answer (without the 0xFFFFFFFF header),
// answering a challenge (0x41) if the server sends one first.
func (a *a2sConn) ask(req []byte, challengeAppends bool) ([]byte, error) {
	if _, err := a.c.Write(req); err != nil {
		return nil, err
	}
	ans, err := a.read()
	if err != nil {
		return nil, err
	}
	for i := 0; i < 2 && len(ans) >= 5 && ans[0] == 'A'; i++ {
		ch := ans[1:5]
		var again []byte
		if challengeAppends {
			again = append(append([]byte{}, req...), ch...)
		} else {
			again = append(append([]byte{}, req[:5]...), ch...)
		}
		if _, err := a.c.Write(again); err != nil {
			return nil, err
		}
		if ans, err = a.read(); err != nil {
			return nil, err
		}
	}
	return ans, nil
}

// Info is the A2S_INFO answer.
type Info struct {
	Name       string `json:"name"`
	Map        string `json:"map"`
	Folder     string `json:"folder"`
	Game       string `json:"game"`
	Players    int    `json:"players"`
	MaxPlayers int    `json:"maxPlayers"`
	Password   bool   `json:"password"`
	VAC        bool   `json:"vac"`
	Version    string `json:"version"`
	GamePort   int    `json:"gamePort,omitempty"`
	SteamID    uint64 `json:"steamId,omitempty"`
	Keywords   string `json:"keywords,omitempty"`
}

// reader reads the fields of an answer.
type reader struct {
	b   []byte
	p   int
	err error
}

func (r *reader) u8() byte {
	if r.err != nil || r.p >= len(r.b) {
		r.err = errA2SShort
		return 0
	}
	v := r.b[r.p]
	r.p++
	return v
}

func (r *reader) take(n int) []byte {
	if r.err != nil || n < 0 || r.p+n > len(r.b) {
		r.err = errA2SShort
		return nil
	}
	v := r.b[r.p : r.p+n]
	r.p += n
	return v
}

func (r *reader) u16() uint16 {
	b := r.take(2)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint16(b)
}

func (r *reader) u32() uint32 {
	b := r.take(4)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint32(b)
}

func (r *reader) u64() uint64 {
	b := r.take(8)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint64(b)
}

// cstr reads a 0-terminated string.
func (r *reader) cstr() []byte {
	if r.err != nil {
		return nil
	}
	i := bytes.IndexByte(r.b[r.p:], 0)
	if i < 0 {
		r.err = errA2SShort
		return nil
	}
	v := r.b[r.p : r.p+i]
	r.p += i + 1
	return v
}

func (r *reader) more() bool { return r.err == nil && r.p < len(r.b) }

// parseInfo reads an A2S_INFO answer (starting at its 'I').
func parseInfo(b []byte) (Info, error) {
	r := &reader{b: b}
	if r.u8() != 'I' {
		return Info{}, errA2SUnknown
	}
	r.u8() // protocol
	in := Info{Name: string(r.cstr()), Map: string(r.cstr()), Folder: string(r.cstr()), Game: string(r.cstr())}
	r.u16() // app id
	in.Players, in.MaxPlayers = int(r.u8()), int(r.u8())
	r.u8() // bots
	r.u8() // server type
	r.u8() // environment
	in.Password = r.u8() == 1
	in.VAC = r.u8() == 1
	in.Version = string(r.cstr())
	if r.err != nil {
		return Info{}, fmt.Errorf("A2S_INFO: %w", r.err)
	}
	if r.more() {
		edf := r.u8()
		if edf&0x80 != 0 {
			in.GamePort = int(r.u16())
		}
		if edf&0x10 != 0 {
			in.SteamID = r.u64()
		}
		if edf&0x40 != 0 {
			r.u16()
			r.cstr()
		}
		if edf&0x20 != 0 {
			in.Keywords = string(r.cstr())
		}
	}
	return in, nil
}

// rule is one A2S_RULES entry, as raw bytes (DayZ's mod list is binary).
type rule struct{ key, value []byte }

// parseRules reads an A2S_RULES answer (starting at its 'E').
func parseRules(b []byte) ([]rule, error) {
	r := &reader{b: b}
	if r.u8() != 'E' {
		return nil, errA2SUnknown
	}
	n := int(r.u16())
	out := make([]rule, 0, n)
	for i := 0; i < n && r.more(); i++ {
		k := r.cstr()
		v := r.cstr()
		if r.err != nil {
			break
		}
		out = append(out, rule{k, v})
	}
	return out, nil
}

// queryInfo asks the server at addr for A2S_INFO.
func queryInfo(a *a2sConn) (Info, error) {
	ans, err := a.ask(a2sInfoReq, true)
	if err != nil {
		return Info{}, err
	}
	return parseInfo(ans)
}

// queryRules asks for A2S_RULES.
func queryRules(a *a2sConn) ([]rule, error) {
	ans, err := a.ask(append(append([]byte{}, a2sHeader...), 'V', 0xFF, 0xFF, 0xFF, 0xFF), false)
	if err != nil {
		return nil, err
	}
	return parseRules(ans)
}
