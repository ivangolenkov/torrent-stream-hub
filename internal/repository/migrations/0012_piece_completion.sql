-- Disposable hints only. Current-open verification is always authoritative.
CREATE TABLE piece_completion (
    infohash BLOB NOT NULL,
    piece INTEGER NOT NULL CHECK(piece>=0),
    complete INTEGER NOT NULL CHECK(complete IN (0,1)),
    PRIMARY KEY(infohash,piece)
) WITHOUT ROWID;
