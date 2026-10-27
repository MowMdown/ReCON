package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// flowsTableSQL creates the flows table. The id is a plain INTEGER PRIMARY
// KEY, not AUTOINCREMENT: SaveStoredFlow gives a new flow the lowest free number
// (from 0), so deleting flows frees their numbers for the next ones.
//
// Columns: script holds the steps as JSON, cancel the When stopped steps,
// timer the delay before step 1, schedule the trigger ("manual", "on-join",
// "on-chat" or a cron expression; see the flows package), and
// counter_threshold the flow's {threshold}.
const flowsTableSQL = `CREATE TABLE %s (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	script TEXT NOT NULL,
	cancel TEXT NOT NULL DEFAULT '',
	timer TEXT NOT NULL DEFAULT '',
	schedule TEXT NOT NULL DEFAULT '',
	schedule_enabled INTEGER NOT NULL DEFAULT 0,
	position INTEGER NOT NULL DEFAULT 0,
	counter_threshold INTEGER NOT NULL DEFAULT 0
)`

// EnsureFlowsTable creates the flows table if it doesn't exist yet (a new
// database starts with no flows). A table from an earlier release, made with
// AUTOINCREMENT (which never hands a number out twice), is copied into a new
// one without it. Every flow keeps its number; only new flows reuse the
// numbers of deleted ones.
func (db *Database) EnsureFlowsTable() error {
	var existing string
	err := db.QueryRow("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'flows'").Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = db.Exec(fmt.Sprintf(flowsTableSQL, "flows"))
		return err
	} else if err != nil {
		return err
	}
	if !strings.Contains(strings.ToUpper(existing), "AUTOINCREMENT") {
		if !strings.Contains(existing, "counter_threshold") { // added with flow counters
			_, err = db.Exec("ALTER TABLE flows ADD COLUMN counter_threshold INTEGER NOT NULL DEFAULT 0")
		}
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		fmt.Sprintf(flowsTableSQL, "flows_new"),
		`INSERT INTO flows_new (id, name, script, cancel, timer, schedule, schedule_enabled, position)
			SELECT id, name, script, cancel, timer, schedule, schedule_enabled, position FROM flows`,
		"DROP TABLE flows",
		"ALTER TABLE flows_new RENAME TO flows",
		"DELETE FROM sqlite_sequence WHERE name = 'flows'",
	} {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("moving flows to the new table: %w", err)
		}
	}
	return tx.Commit()
}

// FlowRecord is one row of the flows table. Script and Cancel are the steps
// and When stopped steps as JSON; Schedule is "manual", "on-join",
// "on-chat" or a cron expression; Delay is the time before step 1 as typed.
type FlowRecord struct {
	ID               int64
	Name             string
	Script, Cancel   string
	Delay            string
	Schedule         string
	Enabled          bool
	CounterThreshold int
}

// flowColumns and scanFlow read one flows row.
const flowColumns = "id, name, script, cancel, timer, schedule, schedule_enabled, counter_threshold"

func scanFlow(sc interface{ Scan(...any) error }) (FlowRecord, error) {
	var r FlowRecord
	var en int
	err := sc.Scan(&r.ID, &r.Name, &r.Script, &r.Cancel, &r.Delay, &r.Schedule, &en, &r.CounterThreshold)
	r.Enabled = en != 0
	return r, err
}

// ListStoredFlows returns every flow in display order.
func (db *Database) ListStoredFlows() ([]FlowRecord, error) {
	rows, err := db.Query("SELECT " + flowColumns + " FROM flows ORDER BY position, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FlowRecord{}
	for rows.Next() {
		r, err := scanFlow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetStoredFlow returns one flow, or sql.ErrNoRows.
func (db *Database) GetStoredFlow(id int64) (FlowRecord, error) {
	return scanFlow(db.QueryRow("SELECT "+flowColumns+" FROM flows WHERE id = ?", id))
}

// SaveStoredFlow inserts a new flow (isNew; placed last, with the lowest free
// number from 0) or updates flow r.ID. Updating a missing ID returns
// sql.ErrNoRows.
func (db *Database) SaveStoredFlow(r FlowRecord, isNew bool) (int64, error) {
	en := 0
	if r.Enabled {
		en = 1
	}
	if isNew {
		// One statement, so two flows saved at once can't take the same number.
		res, err := db.Exec(`
			INSERT INTO flows (id, name, script, cancel, timer, schedule, schedule_enabled, position, counter_threshold)
			VALUES (
				(SELECT CASE
					WHEN NOT EXISTS (SELECT 1 FROM flows WHERE id = 0) THEN 0
					ELSE (SELECT MIN(f.id) + 1 FROM flows f WHERE NOT EXISTS (SELECT 1 FROM flows g WHERE g.id = f.id + 1))
				END),
				?, ?, ?, ?, ?, ?,
				(SELECT COALESCE(MAX(position), 0) + 1 FROM flows),
				?
			)`,
			r.Name, r.Script, r.Cancel, r.Delay, r.Schedule, en, r.CounterThreshold)
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}
	res, err := db.Exec("UPDATE flows SET name = ?, script = ?, cancel = ?, timer = ?, schedule = ?, schedule_enabled = ?, counter_threshold = ? WHERE id = ?",
		r.Name, r.Script, r.Cancel, r.Delay, r.Schedule, en, r.CounterThreshold, r.ID)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, sql.ErrNoRows
	}
	return r.ID, nil
}

// SetFlowScheduleEnabled turns a flow's trigger (schedule or join) on or off.
func (db *Database) SetFlowScheduleEnabled(id int64, on bool) error {
	en := 0
	if on {
		en = 1
	}
	_, err := db.Exec("UPDATE flows SET schedule_enabled = ? WHERE id = ?", en, id)
	return err
}

// ReorderStoredFlows stores the display order: ids[0] becomes position 1, and so on.
func (db *Database) ReorderStoredFlows(ids []int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, id := range ids {
		if _, err := tx.Exec("UPDATE flows SET position = ? WHERE id = ?", i+1, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteStoredFlow removes a flow row.
func (db *Database) DeleteStoredFlow(id int64) error {
	_, err := db.Exec("DELETE FROM flows WHERE id = ?", id)
	return err
}
