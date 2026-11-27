package economy

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

type Store struct { db *sql.DB }

func NewStore(db *sql.DB) (*Store, error) {
	if db == nil { return nil, errors.New("economy: database is required") }
	schema := "CREATE TABLE IF NOT EXISTS economy_files (" +
		"path TEXT PRIMARY KEY, source_content TEXT NOT NULL DEFAULT '', working_content TEXT NOT NULL DEFAULT '', " +
		"source_hash TEXT NOT NULL DEFAULT '', working_hash TEXT NOT NULL DEFAULT '', updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP);" +
		"CREATE TABLE IF NOT EXISTS economy_revisions (" +
		"id INTEGER PRIMARY KEY AUTOINCREMENT, path TEXT NOT NULL, content TEXT NOT NULL, content_hash TEXT NOT NULL, " +
		"reason TEXT NOT NULL DEFAULT 'edit', created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP);" +
		"CREATE INDEX IF NOT EXISTS idx_economy_revisions_path ON economy_revisions(path, id DESC);"
	if _, err := db.Exec(schema); err != nil { return nil, fmt.Errorf("economy: initialize schema: %w", err) }
	return &Store{db: db}, nil
}

func contentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func (s *Store) ListFiles() ([]File, error) {
	rows, err := s.db.Query("SELECT path, source_hash, working_hash, updated_at FROM economy_files ORDER BY path")
	if err != nil { return nil, err }
	defer rows.Close()
	var files []File
	for rows.Next() {
		var f File
		var updated string
		if err := rows.Scan(&f.Path, &f.SourceHash, &f.WorkingHash, &updated); err != nil { return nil, err }
		f.HasChanges = f.WorkingHash != "" && f.WorkingHash != f.SourceHash
		if t, err := time.Parse("2006-01-02 15:04:05", updated); err == nil { f.UpdatedAt = t }
		files = append(files, f)
	}
	return files, rows.Err()
}

func (s *Store) GetFile(path string) (string, string, error) {
	var source, working string
	err := s.db.QueryRow("SELECT source_content, working_content FROM economy_files WHERE path = ?", path).Scan(&source, &working)
	return source, working, err
}

// ImportSource records an imported baseline. Existing uncommitted editor work
// is retained instead of being silently replaced by a re-import.
func (s *Store) ImportSource(path, content string) error {
	if path == "" { return errors.New("mission file path is required") }
	if _, err := ParseXML(content); err != nil { return fmt.Errorf("invalid XML in %s: %w", path, err) }
	hash := contentHash(content)
	_, err := s.db.Exec(
		"INSERT INTO economy_files(path, source_content, working_content, source_hash, working_hash) VALUES(?, ?, ?, ?, ?) "+
			"ON CONFLICT(path) DO UPDATE SET "+
			"source_content=excluded.source_content, "+
			"working_content=CASE WHEN economy_files.working_hash=economy_files.source_hash THEN excluded.working_content ELSE economy_files.working_content END, "+
			"source_hash=excluded.source_hash, "+
			"working_hash=CASE WHEN economy_files.working_hash=economy_files.source_hash THEN excluded.working_hash ELSE economy_files.working_hash END, "+
			"updated_at=CURRENT_TIMESTAMP",
		path, content, content, hash, hash)
	return err
}

func (s *Store) SaveWorkingCopy(path, content, expectedSourceHash string) error {
	if path == "" { return errors.New("mission file path is required") }
	if _, err := ParseXML(content); err != nil { return fmt.Errorf("refusing to save invalid XML: %w", err) }
	tx, err := s.db.Begin()
	if err != nil { return err }
	defer tx.Rollback()
	var sourceHash string
	if err := tx.QueryRow("SELECT source_hash FROM economy_files WHERE path = ?", path).Scan(&sourceHash); err != nil { return err }
	if expectedSourceHash != "" && sourceHash != expectedSourceHash {
		return errors.New("mission file changed since this edit was loaded; reload or compare before saving")
	}
	hash := contentHash(content)
	if _, err := tx.Exec("UPDATE economy_files SET working_content=?, working_hash=?, updated_at=CURRENT_TIMESTAMP WHERE path=?", content, hash, path); err != nil { return err }
	if _, err := tx.Exec("INSERT INTO economy_revisions(path, content, content_hash, reason) VALUES(?, ?, ?, 'edit')", path, content, hash); err != nil { return err }
	return tx.Commit()
}

func (s *Store) ChangedFiles() ([]struct{ Path, Content string }, error) {
	rows, err := s.db.Query("SELECT path, working_content FROM economy_files WHERE working_hash != source_hash ORDER BY path")
	if err != nil { return nil, err }
	defer rows.Close()
	var out []struct{ Path, Content string }
	for rows.Next() {
		var item struct{ Path, Content string }
		if err := rows.Scan(&item.Path, &item.Content); err != nil { return nil, err }
		out = append(out, item)
	}
	return out, rows.Err()
}
