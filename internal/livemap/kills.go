package livemap

import (
	"database/sql"
	"time"
)

// db is ReCON's database; recon-mod keeps its own table in it (mod_kills).
var db *sql.DB

// killsSchema is recon-mod's table: the deaths the mod reported. killer_guid
// is ” for the environment, an infected or an animal, and the victim's own
// GUID for a suicide.
const killsSchema = `
CREATE TABLE IF NOT EXISTS mod_kills (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	at DATETIME NOT NULL,
	victim_guid TEXT NOT NULL,
	victim_name TEXT NOT NULL,
	killer_guid TEXT NOT NULL,
	killer_name TEXT NOT NULL,
	weapon TEXT NOT NULL,
	distance REAL NOT NULL,
	x REAL NOT NULL,
	z REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_mod_kills_victim ON mod_kills(victim_guid);
CREATE INDEX IF NOT EXISTS idx_mod_kills_killer ON mod_kills(killer_guid);`

// openKills creates the table if needed and uses d from then on.
func openKills(d *sql.DB) error {
	if _, err := d.Exec(killsSchema); err != nil {
		return err
	}
	db = d
	return nil
}

// Kill is one death recon-mod reported.
type Kill struct {
	At         time.Time `json:"at"`
	VictimGUID string    `json:"victimGuid"`
	VictimName string    `json:"victimName"`
	KillerGUID string    `json:"killerGuid"` // "" for the environment, an infected or an animal
	KillerName string    `json:"killerName"`
	Weapon     string    `json:"weapon"` // class name: a weapon, a vehicle, an infected...
	Distance   float64   `json:"distance"`
	X          float64   `json:"x"`
	Z          float64   `json:"z"`
}

// addKill stores a death.
func addKill(k Kill) error {
	_, err := db.Exec(`INSERT INTO mod_kills (at, victim_guid, victim_name, killer_guid, killer_name, weapon, distance, x, z)
		VALUES (?, lower(?), ?, lower(?), ?, ?, ?, ?, ?)`,
		k.At, k.VictimGUID, k.VictimName, k.KillerGUID, k.KillerName, k.Weapon, k.Distance, k.X, k.Z)
	return err
}

// killCounts is a player's kills of other players (suicides aside) and deaths.
func killCounts(guid string) (kills, deaths int, err error) {
	err = db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM mod_kills WHERE killer_guid = lower(?1) AND victim_guid != killer_guid AND victim_guid != ''),
		(SELECT COUNT(*) FROM mod_kills WHERE victim_guid = lower(?1))`, guid).Scan(&kills, &deaths)
	return
}

// killsOf is the latest limit deaths a player was in, as killer or victim,
// newest first.
func killsOf(guid string, limit int) ([]Kill, error) {
	rows, err := db.Query(`SELECT at, victim_guid, victim_name, killer_guid, killer_name, weapon, distance, x, z
		FROM mod_kills WHERE victim_guid = lower(?1) OR killer_guid = lower(?1) ORDER BY id DESC LIMIT ?2`, guid, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Kill{}
	for rows.Next() {
		var k Kill
		if err := rows.Scan(&k.At, &k.VictimGUID, &k.VictimName, &k.KillerGUID, &k.KillerName, &k.Weapon, &k.Distance, &k.X, &k.Z); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
