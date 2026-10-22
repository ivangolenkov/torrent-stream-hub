-- Independent of runtime snapshots; clearing requires an explicit Resume.
CREATE TABLE torrent_download_blocks (
    hash TEXT PRIMARY KEY,
    reason TEXT NOT NULL
);
INSERT INTO torrent_download_blocks(hash, reason)
SELECT hash, 'Disk full' FROM torrents WHERE state = 'DiskFull';
