package serverinfo

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Keywords is what DayZ puts in A2S_INFO's keywords, a comma-separated
// list such as "battleye,external,privHive,shard,lqs0,etm2.000000,
// entm4.000000,mod,14:59". A 3PP server also lists "no3rd" (checked on a
// live server: despite the name, it is there when third person is allowed,
// and missing on a 1PP-only server).
type Keywords struct {
	Time        string  `json:"time"`        // in-game time, "14:59"
	DayAccel    float64 `json:"dayAccel"`    // etm: serverTimeAcceleration
	NightAccel  float64 `json:"nightAccel"`  // entm: serverNightTimeAcceleration (on top of DayAccel)
	FirstPerson bool    `json:"firstPerson"` // 1PP only: no "no3rd" in the list
	BattlEye    bool    `json:"battleye"`
	Queue       int     `json:"queue"` // lqs: players waiting to join
	Modded      bool    `json:"modded"`
}

var clockRe = regexp.MustCompile(`^\d{1,2}:\d{2}$`)

func parseKeywords(s string) Keywords {
	k := Keywords{DayAccel: 1, NightAccel: 1, FirstPerson: true}
	for _, w := range strings.Split(s, ",") {
		w = strings.TrimSpace(w)
		switch {
		case clockRe.MatchString(w):
			k.Time = w
		case w == "no3rd":
			k.FirstPerson = false // third person allowed
		case w == "battleye":
			k.BattlEye = true
		case w == "mod":
			k.Modded = true
		case strings.HasPrefix(w, "entm"):
			if f, err := strconv.ParseFloat(w[4:], 64); err == nil && f > 0 {
				k.NightAccel = f
			}
		case strings.HasPrefix(w, "etm"):
			if f, err := strconv.ParseFloat(w[3:], 64); err == nil && f > 0 {
				k.DayAccel = f
			}
		case strings.HasPrefix(w, "lqs"):
			k.Queue, _ = strconv.Atoi(w[3:])
		}
	}
	return k
}

// Mod is one mod the server runs, from A2S_RULES.
type Mod struct {
	WorkshopID uint64 `json:"workshopId,string"`
	Name       string `json:"name"`
	DLC        bool   `json:"dlc,omitempty"`
}

// decodeMods reads DayZ's mod list from A2S_RULES. DayZ sends it as binary
// split over rules whose key is 2 bytes below 0x20 (chunk number, count),
// with 0x00, 0x01 and 0xFF escaped as 0x01 0x02, 0x01 0x01 and 0x01 0x03.
// Put together: a 4-byte version, the mod count, then per mod a 4-byte
// hash, a byte whose low 4 bits are the Workshop ID's length (0x10 marks a
// DLC), the ID (little-endian), and the name (length-prefixed). The format
// is the one Arma 3 uses (Bohemia's ServerBrowserProtocol3); it isn't
// documented for DayZ, so anything that doesn't fit gives no mods rather
// than wrong ones.
func decodeMods(rules []rule) ([]Mod, bool) {
	type chunk struct {
		n    int
		data []byte
	}
	var chunks []chunk
	for _, r := range rules {
		if len(r.key) == 2 && r.key[0] < 0x20 && r.key[1] < 0x20 {
			chunks = append(chunks, chunk{int(r.key[0]), r.value})
		}
	}
	if len(chunks) == 0 {
		return nil, false
	}
	sort.SliceStable(chunks, func(a, b int) bool { return chunks[a].n < chunks[b].n })
	var raw []byte
	for _, c := range chunks {
		raw = append(raw, c.data...)
	}
	data := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		if raw[i] == 0x01 && i+1 < len(raw) {
			switch raw[i+1] {
			case 0x01:
				data = append(data, 0x01)
				i++
				continue
			case 0x02:
				data = append(data, 0x00)
				i++
				continue
			case 0x03:
				data = append(data, 0xFF)
				i++
				continue
			}
		}
		data = append(data, raw[i])
	}
	r := &reader{b: data}
	r.u32() // version, flags
	n := int(r.u8())
	mods := make([]Mod, 0, n)
	for i := 0; i < n; i++ {
		r.u32() // hash
		flags := r.u8()
		idLen := int(flags & 0x0F)
		if idLen > 8 {
			return nil, false
		}
		var id uint64
		for j, b := range r.take(idLen) {
			id |= uint64(b) << (8 * j)
		}
		name := r.take(int(r.u8()))
		if r.err != nil {
			return nil, false
		}
		mods = append(mods, Mod{WorkshopID: id, Name: string(name), DLC: flags&0x10 != 0})
	}
	return mods, true
}
