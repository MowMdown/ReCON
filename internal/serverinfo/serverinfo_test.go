package serverinfo

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/MowMdown/ReCON/internal/steam"
	"github.com/MowMdown/ReCON/internal/store"
	"github.com/MowMdown/ReCON/internal/test"
)

// dayzModRules encodes mods the way DayZ does in A2S_RULES: binary, escaped,
// split into chunks of at most size bytes under 2-byte keys.
func dayzModRules(mods []Mod, size int) []rule {
	var b []byte
	b = append(b, 3, 0, 0, 0) // version and flags
	b = append(b, byte(len(mods)))
	for i, m := range mods {
		b = append(b, byte(i), 0xFF, 0, 1) // a hash with bytes that need escaping
		var id []byte
		for v := m.WorkshopID; v > 0; v >>= 8 {
			id = append(id, byte(v))
		}
		flags := byte(len(id))
		if m.DLC {
			flags |= 0x10
		}
		b = append(b, flags)
		b = append(b, id...)
		b = append(b, byte(len(m.Name)))
		b = append(b, m.Name...)
	}
	b = append(b, 0, 0) // signatures, description
	var esc []byte
	for _, c := range b {
		switch c {
		case 0x01:
			esc = append(esc, 0x01, 0x01)
		case 0x00:
			esc = append(esc, 0x01, 0x02)
		case 0xFF:
			esc = append(esc, 0x01, 0x03)
		default:
			esc = append(esc, c)
		}
	}
	var chunks [][]byte
	for len(esc) > 0 {
		n := min(size, len(esc))
		if esc[n-1] == 0x01 && n < len(esc) { // don't split an escape
			n--
		}
		chunks = append(chunks, esc[:n])
		esc = esc[n:]
	}
	var out []rule
	for i, c := range chunks {
		out = append(out, rule{[]byte{byte(i + 1), byte(len(chunks))}, c})
	}
	return append(out, rule{[]byte("allowedBuild"), []byte("0")})
}

func rulesAnswer(rules []rule) []byte {
	b := append([]byte{0xFF, 0xFF, 0xFF, 0xFF, 'E'}, 0, 0)
	binary.LittleEndian.PutUint16(b[5:], uint16(len(rules)))
	for _, r := range rules {
		b = append(b, r.key...)
		b = append(b, 0)
		b = append(b, r.value...)
		b = append(b, 0)
	}
	return b
}

func infoAnswer(name, mapName, version string, players, max int, gamePort int, keywords string) []byte {
	b := []byte{0xFF, 0xFF, 0xFF, 0xFF, 'I', 17}
	for _, s := range []string{name, mapName, "dayz", "DayZ"} {
		b = append(append(b, s...), 0)
	}
	b = append(b, 0, 0) // app id
	b = append(b, byte(players), byte(max), 0, 'd', 'l', 0, 0)
	b = append(append(b, version...), 0)
	b = append(b, 0x80|0x20)
	b = binary.LittleEndian.AppendUint16(b, uint16(gamePort))
	return append(append(b, keywords...), 0)
}

// fakeQueryServer answers A2S_INFO (after a challenge) and A2S_RULES (in two
// split packets) on a local UDP port.
func fakeQueryServer(t *testing.T, info, rules []byte) int {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	challenge := []byte{0x41, 0x42, 0x43, 0x44}
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			req := buf[:n]
			switch {
			case bytes.HasPrefix(req[4:], []byte("TSource Engine Query\x00")):
				if !bytes.HasSuffix(req, challenge) {
					pc.WriteTo(append([]byte{0xFF, 0xFF, 0xFF, 0xFF, 'A'}, challenge...), addr)
					continue
				}
				pc.WriteTo(info, addr)
			case req[4] == 'V':
				if !bytes.Equal(req[5:9], challenge) {
					pc.WriteTo(append([]byte{0xFF, 0xFF, 0xFF, 0xFF, 'A'}, challenge...), addr)
					continue
				}
				half := len(rules) / 2
				for i, part := range [][]byte{rules[:half], rules[half:]} {
					h := []byte{0xFE, 0xFF, 0xFF, 0xFF, 7, 0, 0, 0, 2, byte(i), 0, 5}
					pc.WriteTo(append(h, part...), addr)
				}
			}
		}
	}()
	return pc.LocalAddr().(*net.UDPAddr).Port
}

var testMods = []Mod{
	{WorkshopID: 1602372402, Name: "DeerIsle"},
	{WorkshopID: 1559212036, Name: "Community Framework"},
	{WorkshopID: 1564026768, Name: "Community-Online-Tools"},
	{WorkshopID: 256, Name: "x"}, // an ID with a 0x00 byte
}

func TestRefreshReadsTheServer(t *testing.T) {
	db := test.OpenDB(t)
	SetDatabase(db)
	t.Cleanup(func() { SetDatabase(nil); state.upID = 0; state.modsChecked = nil })
	asked := 0
	workshopItems = func(ctx context.Context, ids []uint64) (map[uint64]steam.WorkshopItem, error) {
		asked++
		out := map[uint64]steam.WorkshopItem{}
		for _, id := range ids {
			out[id] = steam.WorkshopItem{ID: id, Title: "mod " + strconv.FormatUint(id, 10), SizeBytes: 1000, Updated: 1700000000}
		}
		return out, nil
	}
	t.Cleanup(func() { workshopItems = steam.WorkshopItems })

	info := infoAnswer("Harambe's Safari Island Adventure", "deerisle", "1.30.164289", 2, 60, 2302,
		"battleye,external,privHive,shard,lqs0,etm2.000000,entm4.000000,mod,14:59")
	port := fakeQueryServer(t, info, rulesAnswer(dayzModRules(testMods, 20)))
	state.cfg = Config{QueryHost: "127.0.0.1", QueryPort: port}
	s := Refresh()
	if !s.Answered {
		t.Fatalf("not answered: %s", s.Error)
	}
	if s.Info.Name != "Harambe's Safari Island Adventure" || s.Info.Map != "deerisle" || s.Info.Players != 2 || s.Info.MaxPlayers != 60 || s.Info.GamePort != 2302 || s.Info.Version != "1.30.164289" {
		t.Errorf("info: %+v", s.Info)
	}
	k := s.Keywords
	if k.Time != "14:59" || k.DayAccel != 2 || k.NightAccel != 4 || !k.FirstPerson || !k.BattlEye || k.Queue != 0 {
		t.Errorf("keywords: %+v", k)
	}
	if k := parseKeywords("battleye,no3rd,14:59"); k.FirstPerson {
		t.Error("no3rd read as 1PP; it marks a 3PP server")
	}
	if !s.ModsKnown || len(s.Mods) != len(testMods) {
		t.Fatalf("mods: %v %+v", s.ModsKnown, s.Mods)
	}
	for i, m := range testMods {
		if s.Mods[i] != m {
			t.Errorf("mod %d: %+v, want %+v", i, s.Mods[i], m)
		}
	}
	// Steam was asked, the mods saved as up to date, and not asked again
	// before modCheckEvery.
	rows, _ := db.ModUpdates()
	if asked != 1 || len(rows) != 4 || rows["1559212036"].AckedUpdated != 1700000000 {
		t.Errorf("asked %d, rows %+v", asked, rows)
	}
	Refresh()
	if asked != 1 {
		t.Errorf("Steam asked %d times, want once", asked)
	}
	if n := test.CountRows(t, db, "server_uptime"); n != 1 {
		t.Errorf("%d uptime rows, want 1", n)
	}
}

func TestRefreshWhenTheServerIsDown(t *testing.T) {
	db := test.OpenDB(t)
	SetDatabase(db)
	t.Cleanup(func() { SetDatabase(nil); state.upID = 0 })
	pc, _ := net.ListenPacket("udp", "127.0.0.1:0") // answers nothing
	port := pc.LocalAddr().(*net.UDPAddr).Port
	pc.Close()
	old := queryTimeout
	queryTimeout = 200 * time.Millisecond
	t.Cleanup(func() { queryTimeout = old })
	state.cfg = Config{QueryHost: "127.0.0.1", QueryPort: port}
	state.snap = Snapshot{Answered: true, Info: Info{Name: "kept"}}
	s := Refresh()
	if s.Answered || s.Error == "" || s.Info.Name != "kept" {
		t.Errorf("got %+v, want not answered with the last answer kept", s)
	}
	var up int
	if err := db.QueryRow("SELECT up FROM server_uptime").Scan(&up); err != nil || up != 0 {
		t.Errorf("uptime row up=%d err %v, want one row with up=0", up, err)
	}
}

func TestDecodeModsRejectsGarbage(t *testing.T) {
	if mods, ok := decodeMods([]rule{{[]byte{1, 1}, []byte{0x05, 0, 0, 0, 9, 1}}}); ok {
		t.Errorf("garbage decoded as %+v", mods)
	}
	if _, ok := decodeMods([]rule{{[]byte("allowedBuild"), []byte("0")}}); ok {
		t.Error("no chunks decoded as a mod list")
	}
}

func TestPopulationAndStats(t *testing.T) {
	t0 := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	spans := []store.Span{
		{GUID: "a", Start: at(-30), End: at(30)}, // on at the start
		{GUID: "b", Start: at(10), End: at(20)},
		{GUID: "a", Start: at(30), End: at(40)}, // reconnects at once
		{GUID: "c", Start: at(50), End: at(60)},
	}
	got := Population(spans, t0, at(60))
	want := []Point{{at(0).Unix(), 1}, {at(10).Unix(), 2}, {at(20).Unix(), 1}, {at(40).Unix(), 0}, {at(50).Unix(), 1}, {at(60).Unix(), 0}}
	if len(got) != len(want) {
		t.Fatalf("population %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("point %d: %v, want %v", i, got[i], want[i])
		}
	}
	st := SumUp(spans, t0, at(60))
	// 30 + 10 + 10 + 10 player-minutes over 60 minutes.
	if st.Peak != 2 || st.Unique != 3 || st.Sessions != 4 || st.Average < 0.999 || st.Average > 1.001 {
		t.Errorf("stats %+v", st)
	}
}

func TestBusiest(t *testing.T) {
	loc := time.FixedZone("EST", -5*3600)
	// Two Wednesdays 20:00-21:00 local with 1 player; the range covers two weeks.
	wed := time.Date(2026, 11, 4, 20, 0, 0, 0, loc)
	spans := []store.Span{
		{GUID: "a", Start: wed, End: wed.Add(time.Hour)},
		{GUID: "a", Start: wed.AddDate(0, 0, 7), End: wed.AddDate(0, 0, 7).Add(30 * time.Minute)},
	}
	since := time.Date(2026, 11, 1, 0, 0, 0, 0, loc)
	b := Busiest(spans, since, since.AddDate(0, 0, 14), loc)
	if v := b[time.Wednesday][20]; v < 0.749 || v > 0.751 {
		t.Errorf("Wed 20:00 = %v, want 0.75", v)
	}
	if b[time.Wednesday][21] != 0 || b[time.Tuesday][20] != 0 {
		t.Errorf("other slots not empty")
	}
}

func TestConfigFromEnv(t *testing.T) {
	for _, k := range []string{"QUERY_HOST", "QUERY_PORT", "SERVER_IP", "SERVER_TIME", "SERVER_LATITUDE"} {
		t.Setenv(k, "")
	}
	c := ConfigFromEnv("127.0.0.1")
	if c.QueryHost != "127.0.0.1" || c.QueryPort != 27016 || c.PublicIP != "" || c.ServerTime != "SystemTime" || !math.IsNaN(c.Latitude) {
		t.Errorf("defaults: %+v", c)
	}
	if c := ConfigFromEnv("173.27.253.74"); c.PublicIP != "173.27.253.74" {
		t.Errorf("a public RCON host isn't the public IP: %+v", c)
	}
	t.Setenv("QUERY_PORT", "27017")
	t.Setenv("SERVER_IP", "203.0.113.5")
	t.Setenv("SERVER_TIME", "2000/8/31/06/00")
	t.Setenv("SERVER_LATITUDE", "66.5")
	if c := ConfigFromEnv("127.0.0.1"); c.QueryPort != 27017 || c.PublicIP != "203.0.113.5" || c.ServerTime != "2000/8/31/06/00" || c.Latitude != 66.5 {
		t.Errorf("set: %+v", c)
	}
	t.Setenv("SERVER_TIME", "2000/2/31/06/00")
	t.Setenv("SERVER_LATITUDE", "north")
	if c := ConfigFromEnv("127.0.0.1"); c.ServerTime != "SystemTime" || !math.IsNaN(c.Latitude) {
		t.Errorf("bad values kept: %+v", c)
	}
}

// TestSun checks sunrise and sunset for 31 August: Chernarus's latitude,
// the Arctic Circle, the default for an unknown map, and midnight sun.
func TestSun(t *testing.T) {
	c := Config{ServerTime: "2000/8/31/06/00", Latitude: math.NaN()}
	now := time.Now()
	for _, tc := range []struct {
		mapName   string
		lat       float64
		rise, set string
		polar     string
		mapKnown  bool
	}{
		{"ChernarusPlus", math.NaN(), "05:24", "18:36", "", true},
		{"deerisle", math.NaN(), "05:24", "18:36", "", false}, // the default 45°N
		{"namalsk", 66.56, "04:37", "19:23", "", true},
		{"anything", 85, "", "", "day", true},
	} {
		c.Latitude = tc.lat
		s := SunFor(c, tc.mapName, now)
		if s.Sunrise != tc.rise || s.Sunset != tc.set || s.Polar != tc.polar || s.MapKnown != tc.mapKnown || s.Date != "08-31" {
			t.Errorf("%s at %v: %+v", tc.mapName, tc.lat, s)
		}
	}
	// SystemTime uses today's date.
	if s := SunFor(Config{ServerTime: "SystemTime", Latitude: math.NaN()}, "chernarusplus", now); s.Date != now.Format("01-02") {
		t.Errorf("SystemTime date %s", s.Date)
	}
}

// TestBusiestAcrossDaylightSaving checks a session over the night clocks go
// back (1:00 to 2:00 happens twice): it must finish, and count every hour.
func TestBusiestAcrossDaylightSaving(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip(err)
	}
	start := time.Date(2026, 11, 1, 0, 30, 0, 0, loc) // 1 Nov 2026: 2:00 EDT becomes 1:00 EST
	spans := []store.Span{{GUID: "a", Start: start, End: start.Add(4 * time.Hour)}}
	done := make(chan [7][24]float64, 1)
	go func() { done <- Busiest(spans, start.Add(-time.Hour), start.Add(5*time.Hour), loc) }()
	select {
	case b := <-done:
		if b[time.Sunday][1] == 0 || b[time.Sunday][2] == 0 {
			t.Errorf("hours around the change not counted: %v", b[time.Sunday][:4])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Busiest never finished across the clock change")
	}
}
