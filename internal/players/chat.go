package players

import (
	"log"
	"sort"
	"strings"

	"github.com/MowMdown/ReCON/internal/battleye"
	"github.com/MowMdown/ReCON/internal/rcon"
	"github.com/MowMdown/ReCON/internal/store"
)

// guidForChat finds who wrote a chat line: an online player with that name,
// else the last player seen with it (someone who chatted and left before the
// next poll). "" when nobody matches.
func guidForChat(name string) string {
	if p, ok := onlineByName(name); ok && p.GUID != "" {
		return p.GUID
	}
	guid, err := database.GUIDByName(name)
	if err != nil {
		log.Printf("chat: looking up %q: %v", name, err)
	}
	return guid
}

// StartChatRecorder files every incoming chat line (rcon.Events)
// under its writer's GUID for as long as ReCON runs. A line from a name no
// player has had is logged and left out of the history (it still shows in
// the console).
func StartChatRecorder() {
	events, _ := rcon.Events.Subscribe(256)
	go func() {
		for ev := range events {
			if ev.Kind != battleye.EventChat {
				continue
			}
			guid := guidForChat(ev.Name)
			if guid == "" {
				log.Printf("chat: no player named %q, line not kept in a history: %s", ev.Name, ev.Text)
				continue
			}
			if err := database.AddChatMessage(ev.Time, guid, store.ChatIn, ev.Text); err != nil {
				log.Printf("chat: store: %v", err)
			}
		}
	}()
}

// onlineChat is an online player's last chat line, with their GUID so the
// caller can check the player number still belongs to the same player.
type onlineChat struct {
	GUID string
	Chat lastChat
}

// LastChatPerPlayer returns the last chat line of every player online who has
// written one this session, by player number.
func LastChatPerPlayer() map[int]onlineChat {
	tracker.Lock()
	defer tracker.Unlock()
	out := map[int]onlineChat{}
	for id, t := range tracker.byID {
		if n := len(t.chats); n > 0 {
			out[id] = onlineChat{t.guid, t.chats[n-1]}
		}
	}
	return out
}

// LatestChatOf is the newest line a player online (by GUID) wrote this session.
func LatestChatOf(guid string) (lastChat, bool) {
	tracker.Lock()
	defer tracker.Unlock()
	for _, t := range tracker.byID {
		if t.guid != "" && strings.EqualFold(t.guid, guid) && len(t.chats) > 0 {
			return t.chats[len(t.chats)-1], true
		}
	}
	return lastChat{}, false
}

// sessionChat is one chat line and who (by GUID) wrote it.
type sessionChat struct {
	GUID string
	Chat lastChat
}

// ChatsAfter returns every chat line players online wrote this session with
// a Seq greater than after, oldest first.
func ChatsAfter(after int64) []sessionChat {
	tracker.Lock()
	defer tracker.Unlock()
	var out []sessionChat
	for _, t := range tracker.byID {
		if t.guid == "" {
			continue
		}
		for _, c := range t.chats {
			if c.Seq > after {
				out = append(out, sessionChat{t.guid, c})
			}
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Chat.Seq < out[b].Chat.Seq })
	return out
}
