package players

import (
	"sort"
	"strings"

	"github.com/MowMdown/ReCON/internal/battleye"
)

// Online is who is online, by player number, as the console log and
// the last poll tell it: the Name has " (Lobby)" while they load and Ping is
// from the last poll. known is false until a poll has listed who is on since
// the server last answered.
func Online() (players []battleye.Player, known bool) {
	tracker.Lock()
	defer tracker.Unlock()
	players = make([]battleye.Player, 0, len(tracker.byID))
	for _, t := range tracker.byID {
		name := t.name
		if t.lobby {
			name += " " + battleye.LobbySuffix
		}
		players = append(players, battleye.Player{ID: t.id, IP: t.ip, Port: t.port, Ping: t.ping, GUID: t.guid, Name: name})
	}
	sort.Slice(players, func(a, b int) bool { return players[a].ID < players[b].ID })
	return players, tracker.known
}

// onlineByName, ByID and ByGUID look a player up in the online
// list (Online), so they never send a command of their own.
func onlineByName(name string) (battleye.Player, bool) {
	return onlineWhere(func(p battleye.Player) bool { return battleye.BareName(p.Name) == name })
}

// ByUniqueName finds who wrote a chat line for the chat commands and
// Player message flows: the one online player with exactly that name. ok is
// false when nobody, or more than one player, has it.
func ByUniqueName(name string) (battleye.Player, bool) {
	players, _ := Online()
	var found battleye.Player
	n := 0
	for _, p := range players {
		if battleye.BareName(p.Name) == name {
			found, n = p, n+1
		}
	}
	return found, n == 1
}

func ByID(id int) (battleye.Player, bool) {
	return onlineWhere(func(p battleye.Player) bool { return p.ID == id })
}

func ByGUID(guid string) (battleye.Player, bool) {
	return onlineWhere(func(p battleye.Player) bool { return strings.EqualFold(p.GUID, guid) })
}

func onlineWhere(match func(battleye.Player) bool) (battleye.Player, bool) {
	players, _ := Online()
	for _, p := range players {
		if match(p) {
			return p, true
		}
	}
	return battleye.Player{}, false
}
