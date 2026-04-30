-- 0001_init.sql

CREATE TABLE IF NOT EXISTS torrents (
    hash TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    size INTEGER NOT NULL,
    downloaded INTEGER NOT NULL DEFAULT 0,
    state TEXT NOT NULL,
    error TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS files (
    hash TEXT NOT NULL,
    "index" INTEGER NOT NULL,
    path TEXT NOT NULL,
    size INTEGER NOT NULL,
    downloaded INTEGER NOT NULL DEFAULT 0,
    priority INTEGER NOT NULL DEFAULT 0,
    is_media BOOLEAN NOT NULL DEFAULT 0,
    PRIMARY KEY (hash, "index"),
    FOREIGN KEY (hash) REFERENCES torrents(hash) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS viewed_history (
    hash TEXT NOT NULL,
    file_index INTEGER NOT NULL,
    viewed_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (hash, file_index)
);
