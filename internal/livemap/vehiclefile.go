package livemap

// vehiclefile.go is a map's vehicles.json: the vehicles the chat commands
// !car, !truck, !boat and !vehicle can spawn, by short name and colour,
// which the admin edits. ReCON writes a first one from the mod's item list.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// VehicleFile is category (car, truck, boat, vehicle) -> short name ->
// colour -> class. Every key is lowercase once loaded, so case never matters.
type VehicleFile map[string]map[string]map[string]string

// Count is how many vehicles (short names) the file has.
func (f VehicleFile) Count() int {
	n := 0
	for _, names := range f {
		n += len(names)
	}
	return n
}

// LoadVehicleFile reads path (world's file). A missing file's error wraps
// os.ErrNotExist; a file that doesn't parse says where.
func LoadVehicleFile(path, world string) (VehicleFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw map[string]map[string]map[string]string
	if err := json.Unmarshal(data, &raw); err != nil {
		var off int64 = -1
		var se *json.SyntaxError
		var te *json.UnmarshalTypeError
		if errors.As(err, &se) {
			off = se.Offset
		} else if errors.As(err, &te) {
			off = te.Offset
		}
		line := 1
		if off > 0 {
			line += bytes.Count(data[:min(int(off), len(data))], []byte("\n"))
		}
		return nil, fmt.Errorf("vehicles.json for %s has an error at line %d: %v", world, line, err)
	}
	f := VehicleFile{}
	for cat, names := range raw {
		c := strings.ToLower(cat)
		if f[c] == nil {
			f[c] = map[string]map[string]string{}
		}
		for name, colours := range names {
			n := strings.ToLower(name)
			if f[c][n] == nil {
				f[c][n] = map[string]string{}
			}
			for colour, class := range colours {
				f[c][n][strings.ToLower(colour)] = class
			}
		}
	}
	return f, nil
}

// vanillaCars are DayZ's own cars: the only classes the first file puts
// under car without a word in the name to go on.
var vanillaCars = map[string]bool{"civiliansedan": true, "sedan_02": true, "hatchback_02": true, "offroadhatchback": true, "offroad_02": true}

// GenerateVehicleFile makes the first vehicles.json from the item list's
// vehicles. Classes that share a display name are one vehicle in its
// colours; anything it can't place surely goes under vehicle, the catch-all.
func GenerateVehicleFile(items []CatalogItem) VehicleFile {
	groups := map[string][]string{}
	names := map[string]string{}
	for _, it := range items {
		if it.Kind != "vehicle" {
			continue
		}
		key := it.Name
		if key == "" || strings.HasPrefix(key, "$") {
			key = "class:" + it.Type // untranslated: alone, by class
		}
		groups[key] = append(groups[key], it.Type)
		names[key] = it.Name
	}

	type entry struct {
		words   []string
		take    int
		colours map[string]string
	}
	byCat := map[string][]*entry{}
	keysSorted := make([]string, 0, len(groups))
	for k := range groups {
		keysSorted = append(keysSorted, k)
	}
	sort.Strings(keysSorted)
	for _, k := range keysSorted {
		classes := groups[k]
		sort.Strings(classes)
		base := vehicleBase(classes)
		colours := map[string]string{}
		for _, c := range classes {
			col := "default"
			if len(classes) > 1 && c != base {
				col = strings.ToLower(strings.TrimPrefix(c, base+"_"))
			}
			colours[col] = c
		}
		words := []string{}
		if !strings.HasPrefix(k, "class:") {
			for _, w := range strings.Fields(names[k]) {
				if w = cleanName(w); w != "" {
					words = append(words, w)
				}
			}
		}
		if len(words) == 0 {
			words = []string{cleanName(base)}
		}
		byCat[vehicleCategory(base)] = append(byCat[vehicleCategory(base)], &entry{words: words, take: 1, colours: colours})
	}

	f := VehicleFile{}
	for cat, es := range byCat {
		// Two with one name both take their next word until they differ;
		// out of words, a number.
		name := func(e *entry) string { return strings.Join(e.words[:min(e.take, len(e.words))], "") }
		for {
			seen := map[string][]*entry{}
			for _, e := range es {
				seen[name(e)] = append(seen[name(e)], e)
			}
			grew := false
			for _, same := range seen {
				if len(same) < 2 {
					continue
				}
				for _, e := range same {
					if e.take < len(e.words) {
						e.take++
						grew = true
					}
				}
			}
			if !grew {
				break
			}
		}
		f[cat] = map[string]map[string]string{}
		for _, e := range es {
			n := name(e)
			for i := 2; f[cat][n] != nil; i++ {
				n = name(e) + fmt.Sprint(i)
			}
			f[cat][n] = e.colours
		}
	}
	return f
}

// vehicleBase is what a vehicle's classes share: their common prefix if
// it is one of them (CivilianSedan), else cut back to before its last _
// (Boat_01_Black, Boat_01_Blue -> Boat_01).
func vehicleBase(classes []string) string {
	p := classes[0]
	for _, c := range classes[1:] {
		for !strings.HasPrefix(c, p) {
			p = p[:len(p)-1]
		}
	}
	for _, c := range classes {
		if c == p {
			return p
		}
	}
	if i := strings.LastIndex(p, "_"); i > 0 {
		return p[:i]
	}
	return p
}

func vehicleCategory(base string) string {
	b := strings.ToLower(base)
	switch {
	case strings.Contains(b, "boat"):
		return "boat"
	case strings.Contains(b, "truck"):
		return "truck"
	case vanillaCars[b]:
		return "car"
	}
	return "vehicle"
}

// cleanName keeps a word's letters and digits, lowercased.
func cleanName(w string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(w) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// CatalogClass is the item list's entry for class, ignoring case (the
// file may spell it any way; the mod needs its exact spelling).
func CatalogClass(class string, now time.Time) (CatalogItem, bool) {
	state.Lock()
	defer state.Unlock()
	if !connected(now) || state.catalogTypes == nil {
		return CatalogItem{}, false
	}
	if it, ok := state.catalogTypes[class]; ok {
		return it, true
	}
	for t, it := range state.catalogTypes {
		if strings.EqualFold(t, class) {
			return it, true
		}
	}
	return CatalogItem{}, false
}
