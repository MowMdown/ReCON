package store

import (
	"strings"
	"time"
)

// Chat directions.
const (
	ChatIn  = "in"  // the player wrote it
	ChatOut = "out" // ReCON sent it to them
)

// chatLine is one row of a player's history. Line is ready to show:
// "(Global) Name: text" for chat in, "> say 3 text" for messages out.
type chatLine struct {
	Time      time.Time `json:"time"`
	Direction string    `json:"direction"`
	Line      string    `json:"line"`
}

// AddChatMessage stores one line of a player's history.
func (db *Database) AddChatMessage(at time.Time, guid, direction, line string) error {
	_, err := db.Exec("INSERT INTO chat_messages (at, guid, direction, line) VALUES (?, ?, ?, ?)",
		at.UTC(), strings.ToLower(guid), direction, line)
	return err
}

// GetChatHistory returns a player's newest `limit` lines, oldest first.
func (db *Database) GetChatHistory(guid string, limit int) ([]chatLine, error) {
	rows, err := db.Query(`
		SELECT at, direction, line FROM (
			SELECT id, at, direction, line FROM chat_messages
			WHERE guid = ? ORDER BY id DESC LIMIT ?
		) ORDER BY id`, strings.ToLower(guid), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	lines := []chatLine{}
	for rows.Next() {
		var l chatLine
		var at any
		if err := rows.Scan(&at, &l.Direction, &l.Line); err != nil {
			return nil, err
		}
		l.Time = time.Unix(unixOf(at), 0)
		lines = append(lines, l)
	}
	return lines, rows.Err()
}
