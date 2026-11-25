package flows

// vehicles.go is !car, !truck, !boat and !vehicle: a mod or admin spawns a
// whole vehicle in front of themselves, picked by short name and color
// from the map's vehicles.json (internal/livemap/vehiclefile.go).

import (
	"errors"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/game"
	"github.com/MowMdown/ReCON/internal/livemap"
)

// The mod side, as package vars so tests can stand in for it.
var (
	modConnected   = func() bool { s, _ := livemap.Current(time.Now()); return s.Connected }
	vehicleChoices = func() (livemap.VehicleFile, string, error) { return livemap.VehicleChoices(time.Now()) }
	// spawnVehicle queues one whole vehicle of class in front of the player
	// (guid), replacing their previous chat car, and returns its display name. The class may be spelled in any
	// case; the mod gets the item list's spelling.
	spawnVehicle = func(guid, class string) (string, error) {
		now := time.Now()
		it, ok := livemap.CatalogClass(class, now)
		if !ok {
			return "", errors.New(class + " isn't an item the server can spawn")
		}
		// Replaces their last chat car (livemap/chatcars.go).
		if _, err := livemap.SpawnChatVehicle(guid, it.Type, now); err != nil {
			return "", err
		}
		if it.Name == "" || strings.HasPrefix(it.Name, "$") {
			return it.Type, nil
		}
		return it.Name, nil
	}
	pickColor = func(n int) int { return rand.IntN(n) }
	// removeChatVehicle queues the removal of the player's chat car and
	// returns its display name (livemap.ErrNoChatVehicle: they have none).
	removeChatVehicle = func(guid string) (string, error) {
		now := time.Now()
		_, class, err := livemap.RemoveChatVehicle(guid, now)
		if err != nil {
			return "", err
		}
		if it, ok := livemap.CatalogClass(class, now); ok && it.Name != "" && !strings.HasPrefix(it.Name, "$") {
			return it.Name, nil
		}
		return class, nil
	}
)

var vehicleCategoryText = map[string]string{"car": "Cars", "truck": "Trucks", "boat": "Boats", "vehicle": "Vehicles"}

// runVehicleCommand runs !<cmd> [name] [color] for p. Every answer goes
// to p alone.
func runVehicleCommand(cmd string, p battleye.Player, set chatSetting, args []string) {
	file, _, err := vehicleChoices()
	if err != nil {
		chatSay(p, false, err.Error())
		return
	}
	names := file[cmd]
	list := func(only string) {
		var entries []string
		for _, n := range sortedKeys(names) {
			if only != "" && n != only {
				continue
			}
			var cols []string
			for _, c := range sortedKeys(names[n]) {
				if c != "default" {
					cols = append(cols, c)
				}
			}
			e := n
			if len(cols) > 0 {
				e += " (" + strings.Join(cols, ", ") + ")"
			}
			entries = append(entries, e)
		}
		if len(entries) == 0 {
			entries = []string{"none yet: add some to vehicles.json"}
		}
		var lines []string
		for i := 0; i < len(entries); i += 5 {
			lines = append(lines, fillTemplate(set.Msgs["list"], map[string]string{
				"category": vehicleCategoryText[cmd], "names": strings.Join(entries[i:min(i+5, len(entries))], ", ")}))
		}
		chatSay(p, false, lines[0])
		if len(lines) > 1 { // the rest a few seconds apart, as !players
			go func() {
				for _, l := range lines[1:] {
					time.Sleep(playersLinePause)
					asker, ok := game.PlayerByGUID(p.GUID)
					if !ok {
						return
					}
					chatSay(asker, false, l)
				}
			}()
		}
	}
	if len(args) == 0 {
		list("")
		return
	}
	name := strings.ToLower(args[0])
	colors, ok := names[name]
	if !ok || len(colors) == 0 {
		list("")
		return
	}
	color := "default"
	if len(args) > 1 {
		color = strings.ToLower(args[1])
	} else if _, ok := colors["default"]; !ok {
		cs := sortedKeys(colors)
		color = cs[pickColor(len(cs))]
	}
	class, ok := colors[color]
	if !ok {
		list(name)
		return
	}
	shown, err := spawnVehicle(p.GUID, class)
	if err != nil {
		chatSay(p, false, err.Error())
		return
	}
	if chatSay(p, false, fillTemplate(set.Msgs["spawned"], map[string]string{"vehicle": shown, "color": color, "colour": color /* messages saved before {color} */})) {
		markUsed(cmd)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
