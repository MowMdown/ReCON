package flows

// loadouts.go is !save and !equip: a mod or admin keeps their current gear
// as their loadout, and gets it back later (their gear replaced, every item
// pristine) after typing confirm.

import (
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/livemap"
)

// The mod side, as package vars so tests can stand in for it.
var (
	saveLoadout  = func(guid string) (int, error) { return livemap.SaveLoadout(guid) }
	loadoutCount = func(guid string) (int, error) {
		_, n, err := livemap.Loadout(guid)
		return n, err
	}
	equipLoadout = func(guid string) (int, error) { return livemap.EquipLoadout(guid, time.Now()) }
)

// equipConfirmFor is how long !equip waits for confirm.
var equipConfirmFor = 30 * time.Second

// equipPending is who typed !equip and hasn't answered yet (by GUID: the
// answer's deadline and the command's settings).
var equipPending = struct {
	sync.Mutex
	m map[string]equipAsk
}{m: map[string]equipAsk{}}

type equipAsk struct {
	until time.Time
	set   chatSetting
	n     int
}

func runSaveCommand(p battleye.Player, set chatSetting) {
	go func() {
		n, err := saveLoadout(p.GUID)
		if err != nil {
			chatSay(p, false, "Loadout not saved: "+err.Error())
			return
		}
		if chatSay(p, false, fillTemplate(set.Msgs["saved"], map[string]string{"count": strconv.Itoa(n)})) {
			markUsed("save")
		}
	}()
}

func runEquipCommand(p battleye.Player, set chatSetting) {
	n, err := loadoutCount(p.GUID)
	if errors.Is(err, livemap.ErrNoLoadout) {
		chatSay(p, false, fillTemplate(set.Msgs["none"], nil))
		return
	}
	if err != nil {
		chatSay(p, false, "Loadout not equipped: "+err.Error())
		return
	}
	guid := strings.ToLower(p.GUID)
	until := time.Now().Add(equipConfirmFor)
	equipPending.Lock()
	equipPending.m[guid] = equipAsk{until: until, set: set, n: n}
	equipPending.Unlock()
	if !chatSay(p, false, fillTemplate(set.Msgs["confirm"], map[string]string{"count": strconv.Itoa(n)})) {
		equipPending.Lock()
		delete(equipPending.m, guid)
		equipPending.Unlock()
		return
	}
	markUsed("equip")
	go func() {
		time.Sleep(time.Until(until) + 100*time.Millisecond)
		equipPending.Lock()
		ask, ok := equipPending.m[guid]
		if ok && !time.Now().Before(ask.until) {
			delete(equipPending.m, guid)
		} else {
			ok = false
		}
		equipPending.Unlock()
		if ok {
			chatSay(p, false, fillTemplate(ask.set.Msgs["canceled"], nil))
		}
	}()
}

// answerEquip takes p's chat line while their !equip waits for confirm: it
// reports whether the line was that answer (confirm, or anything else,
// which cancels). Another !equip asks again instead.
func answerEquip(p battleye.Player, message string) bool {
	guid := strings.ToLower(p.GUID)
	msg := strings.ToLower(strings.TrimSpace(message))
	equipPending.Lock()
	ask, ok := equipPending.m[guid]
	if !ok || time.Now().After(ask.until) || msg == "!equip" {
		equipPending.Unlock()
		return false
	}
	delete(equipPending.m, guid)
	equipPending.Unlock()
	if msg != "confirm" {
		chatSay(p, false, fillTemplate(ask.set.Msgs["canceled"], nil))
		return true
	}
	n, err := equipLoadout(p.GUID)
	if err != nil {
		chatSay(p, false, "Loadout not equipped: "+err.Error())
		return true
	}
	chatSay(p, false, fillTemplate(ask.set.Msgs["equipped"], map[string]string{"count": strconv.Itoa(n)}))
	return true
}
