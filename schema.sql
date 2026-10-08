CREATE TABLE IF NOT EXISTS players (
    guid TEXT PRIMARY KEY,
    last_name TEXT NOT NULL,
    last_ip TEXT NOT NULL,
    first_seen DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_seen DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    total_playtime_seconds INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS active_sessions (
    guid TEXT PRIMARY KEY,
    ip TEXT NOT NULL,
    connected_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY(guid) REFERENCES players(guid) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS session_history (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    guid TEXT NOT NULL,
    ip TEXT NOT NULL,
    connected_at DATETIME NOT NULL,
    disconnected_at DATETIME NOT NULL,
    duration_seconds INTEGER NOT NULL,
    FOREIGN KEY(guid) REFERENCES players(guid) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS bans (
    guid TEXT PRIMARY KEY,
    player_name TEXT NOT NULL,
    ip TEXT NOT NULL,
    reason TEXT NOT NULL,
    perm INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_session_history_guid
    ON session_history(guid);

CREATE INDEX IF NOT EXISTS idx_bans_player_name
    ON bans(player_name);