package steam

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// environmentOrDefault returns the value of the environment variable `key`,
// or `def` when the variable is unset or empty.
func environmentOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// steamTableDir is where to look for a steam2guid table: STEAM_TABLE_DIR if
// set (any folder), otherwise the database's folder, so the part files sit
// next to players.db (/data with the default DB_PATH).
var steamTableDir = func() string {
	if d := strings.TrimSpace(os.Getenv("STEAM_TABLE_DIR")); d != "" {
		return d
	}
	return filepath.Dir(environmentOrDefault("DB_PATH", "/data/players.db"))
}()

const (
	steamTableMagic      = "STM2GUID"
	steamTableVersion    = 1
	steamTableHeaderSize = 64
	steamTableGlob       = "steam2guid-*.bin"
	steamTableMinBits    = 4
	steamTableMaxBits    = 30
)

type steamTableHeader struct {
	entryBytes, indexBits int
	part, parts           int
	maxAccount            uint64
	firstGroup, groups    uint32
	entries, total        uint64
}

func (h steamTableHeader) entriesOffset() int64 { return steamTableHeaderSize + 4*int64(h.groups) }

func (h steamTableHeader) fileSize() int64 {
	return h.entriesOffset() + int64(h.entries)*int64(h.entryBytes)
}

func parseSteamTableHeader(b []byte) (steamTableHeader, error) {
	var h steamTableHeader
	if len(b) < steamTableHeaderSize || string(b[:8]) != steamTableMagic {
		return h, errors.New("not a steam2guid table (bad magic)")
	}
	le := binary.LittleEndian
	if crc32.ChecksumIEEE(b[:60]) != le.Uint32(b[60:]) {
		return h, errors.New("header checksum mismatch")
	}
	if v := le.Uint16(b[8:]); v != steamTableVersion {
		return h, fmt.Errorf("unsupported table version %d", v)
	}
	if le.Uint64(b[16:]) != steamID64Base {
		return h, errors.New("unexpected SteamID64 base")
	}
	h = steamTableHeader{
		entryBytes: int(b[10]), indexBits: int(b[11]),
		part: int(le.Uint16(b[12:])), parts: int(le.Uint16(b[14:])),
		maxAccount: le.Uint64(b[24:]), firstGroup: le.Uint32(b[32:]),
		groups: le.Uint32(b[36:]), entries: le.Uint64(b[40:]), total: le.Uint64(b[48:]),
	}
	switch {
	case h.entryBytes != 2 && h.entryBytes != 4 && h.entryBytes != 8:
		return h, fmt.Errorf("bad entry size %d", h.entryBytes)
	case h.indexBits < steamTableMinBits || h.indexBits > steamTableMaxBits:
		return h, fmt.Errorf("bad index bits %d", h.indexBits)
	case h.maxAccount == 0 || h.maxAccount > steamSearchRange:
		return h, fmt.Errorf("bad account range %d", h.maxAccount)
	case h.parts == 0 || h.part >= h.parts:
		return h, fmt.Errorf("bad part %d of %d", h.part, h.parts)
	}
	return h, nil
}

type steamTablePart struct {
	f *os.File
	h steamTableHeader
}

// steamTable is an open table.
type steamTable struct {
	parts                 []steamTablePart
	entryBytes, indexBits int
	maxAccount            uint64 // accounts 0 .. maxAccount-1 are covered
	workers               int    // goroutines for a 2-byte lookup
}

// openSteamTable opens the table in steamTableDir, or returns nil when there
// isn't one or it can't be used (logged: the search then runs without it).
func openSteamTable() *steamTable {
	names, _ := filepath.Glob(filepath.Join(steamTableDir, steamTableGlob))
	if len(names) == 0 {
		return nil
	}
	t, err := openSteamTableFiles(names)
	if err != nil {
		log.Printf("steam: ignoring the table in %s: %v", steamTableDir, err)
		return nil
	}
	return t
}

func openSteamTableFiles(names []string) (*steamTable, error) {
	t := &steamTable{workers: 1}
	fail := func(err error) (*steamTable, error) { t.close(); return nil, err }
	for _, name := range names {
		f, err := os.Open(name)
		if err != nil {
			return fail(err)
		}
		t.parts = append(t.parts, steamTablePart{f: f})
		p := &t.parts[len(t.parts)-1]
		buf := make([]byte, steamTableHeaderSize)
		if _, err := f.ReadAt(buf, 0); err != nil {
			return fail(fmt.Errorf("%s: %v", name, err))
		}
		if p.h, err = parseSteamTableHeader(buf); err != nil {
			return fail(fmt.Errorf("%s: %v", name, err))
		}
		if st, err := f.Stat(); err != nil || st.Size() != p.h.fileSize() {
			return fail(fmt.Errorf("%s: wrong size (incomplete or damaged file)", name))
		}
	}
	sort.Slice(t.parts, func(i, j int) bool { return t.parts[i].h.part < t.parts[j].h.part })
	h0 := t.parts[0].h
	t.entryBytes, t.indexBits, t.maxAccount = h0.entryBytes, h0.indexBits, h0.maxAccount
	var next uint64
	var sum uint64
	for i, p := range t.parts {
		h := p.h
		switch {
		case h.part != i || h.parts != len(t.parts):
			return fail(fmt.Errorf("%s: part %d of %d, but found %d part file(s)", p.f.Name(), h.part, h.parts, len(t.parts)))
		case h.entryBytes != h0.entryBytes || h.indexBits != h0.indexBits || h.maxAccount != h0.maxAccount || h.total != h0.total:
			return fail(fmt.Errorf("%s: belongs to a different table", p.f.Name()))
		case uint64(h.firstGroup) != next:
			return fail(fmt.Errorf("%s: starts at group %d, expected %d", p.f.Name(), h.firstGroup, next))
		}
		next += uint64(h.groups)
		sum += h.entries
	}
	if next != uint64(1)<<h0.indexBits || sum != h0.total || sum != h0.maxAccount {
		return fail(errors.New("parts don't cover the whole table"))
	}
	return t, nil
}

func (t *steamTable) close() {
	for _, p := range t.parts {
		if p.f != nil {
			p.f.Close()
		}
	}
}

// steamTableGUIDOf is the raw BattlEye GUID of an account ID.
func steamTableGUIDOf(account uint32) [16]byte {
	var in [10]byte
	in[0], in[1] = 'B', 'E'
	binary.LittleEndian.PutUint64(in[2:], steamID64Base+uint64(account))
	return md5.Sum(in[:])
}

// steamTableSplit returns a GUID's group (its top indexBits bits) and key
// (the next 32 bits).
func steamTableSplit(g *[16]byte, indexBits int) (group, key uint32) {
	u := binary.BigEndian.Uint64(g[:8])
	return uint32(u >> (64 - indexBits)), uint32((u << indexBits) >> 32)
}

// lookup finds the SteamID64 of a hex GUID. found is false when no account
// below maxAccount has that GUID.
func (t *steamTable) lookup(guid string) (steamID64 uint64, found bool, err error) {
	var g [16]byte
	if b, err := hex.DecodeString(strings.TrimSpace(guid)); err != nil || len(b) != 16 {
		return 0, false, fmt.Errorf("GUID %q: want 32 hex characters", guid)
	} else {
		copy(g[:], b)
	}
	group, key := steamTableSplit(&g, t.indexBits)
	i := sort.Search(len(t.parts), func(i int) bool {
		h := t.parts[i].h
		return uint64(h.firstGroup)+uint64(h.groups) > uint64(group)
	})
	if i == len(t.parts) {
		return 0, false, errors.New("group outside the table")
	}
	p := t.parts[i]
	local := group - p.h.firstGroup

	// where the group starts and ends
	var ib [8]byte
	n := 8
	if local+1 == p.h.groups {
		n = 4
	}
	if _, err := p.f.ReadAt(ib[:n], steamTableHeaderSize+4*int64(local)); err != nil {
		return 0, false, fmt.Errorf("%s: index: %v", p.f.Name(), err)
	}
	start := uint64(binary.LittleEndian.Uint32(ib[:]))
	end := p.h.entries
	if n == 8 {
		end = uint64(binary.LittleEndian.Uint32(ib[4:]))
	}
	if end < start || end > p.h.entries {
		return 0, false, fmt.Errorf("%s: damaged index at group %d", p.f.Name(), group)
	}
	if end == start {
		return 0, false, nil
	}
	eb := int64(t.entryBytes)
	buf := make([]byte, int64(end-start)*eb)
	if _, err := p.f.ReadAt(buf, p.h.entriesOffset()+int64(start)*eb); err != nil {
		return 0, false, fmt.Errorf("%s: entries: %v", p.f.Name(), err)
	}

	var acc uint32
	var ok bool
	switch t.entryBytes {
	case 8:
		acc, ok = steamTableLookup8(buf, key, &g)
	case 4:
		acc, ok = steamTableLookup4(buf, key, &g, t.indexBits)
	case 2:
		acc, ok = steamTableLookup2(buf, &g, t.maxAccount, t.workers)
	}
	if !ok {
		return 0, false, nil
	}
	return steamID64Base + uint64(acc), true, nil
}

// steamTableLookup8: entries are [key BE][account LE], sorted by key.
func steamTableLookup8(buf []byte, key uint32, g *[16]byte) (uint32, bool) {
	n := len(buf) / 8
	i := sort.Search(n, func(i int) bool { return binary.BigEndian.Uint32(buf[i*8:]) >= key })
	for ; i < n && binary.BigEndian.Uint32(buf[i*8:]) == key; i++ {
		acc := binary.LittleEndian.Uint32(buf[i*8+4:])
		if steamTableGUIDOf(acc) == *g {
			return acc, true
		}
	}
	return 0, false
}

// steamTableLookup4: entries are account IDs sorted by GUID. GUIDs are
// uniform, so the key predicts where the entry sits (interpolation search);
// after a few probes it falls back to plain bisection.
func steamTableLookup4(buf []byte, key uint32, g *[16]byte, indexBits int) (uint32, bool) {
	lo, hi := 0, len(buf)/4
	loKey, hiKey := uint64(0), uint64(1)<<32 // keys bounding [lo, hi)
	for step := 0; lo < hi; step++ {
		mid := (lo + hi) / 2
		if step < 4 && hiKey > loKey && uint64(key) >= loKey {
			mid = lo + int((uint64(key)-loKey)*uint64(hi-lo)/(hiKey-loKey))
			mid = min(max(mid, lo), hi-1)
		}
		acc := binary.LittleEndian.Uint32(buf[mid*4:])
		h := steamTableGUIDOf(acc)
		c := bytes.Compare(h[:], g[:])
		if c == 0 {
			return acc, true
		}
		_, k := steamTableSplit(&h, indexBits)
		if c < 0 {
			lo, loKey = mid+1, uint64(k)
		} else {
			hi, hiKey = mid, uint64(k)+1
		}
	}
	return 0, false
}

// steamTableLookup2: entries hold only the low 16 bits of each account ID,
// so every possible high half is tried against every entry. Runs on the
// search's cores at lowest thread priority, like the brute force.
func steamTableLookup2(buf []byte, g *[16]byte, maxAccount uint64, workers int) (uint32, bool) {
	lows := make([]uint32, len(buf)/2)
	for i := range lows {
		lows[i] = uint32(binary.LittleEndian.Uint16(buf[i*2:]))
	}
	highs := uint32((maxAccount-1)>>16) + 1
	workers = max(1, min(workers, int(highs)))
	var (
		next   atomic.Uint32
		done   atomic.Bool
		result atomic.Uint32
		wg     sync.WaitGroup
	)
	want0 := binary.LittleEndian.Uint64(g[:8])
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runtime.LockOSThread() // never unlocked: see searchSteamIDs
			lowerThreadPriority()
			var in [10]byte
			in[0], in[1] = 'B', 'E'
			for !done.Load() {
				hi := next.Add(1) - 1
				if hi >= highs {
					return
				}
				for _, lo := range lows {
					acc := uint64(hi)<<16 | uint64(lo)
					if acc >= maxAccount {
						continue
					}
					binary.LittleEndian.PutUint64(in[2:], steamID64Base+acc)
					s := md5.Sum(in[:])
					if binary.LittleEndian.Uint64(s[:8]) == want0 && s == *g {
						result.Store(uint32(acc))
						done.Store(true)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	if !done.Load() {
		return 0, false
	}
	return result.Load(), true
}
