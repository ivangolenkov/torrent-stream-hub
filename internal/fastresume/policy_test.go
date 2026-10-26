package fastresume

import (
	"bytes"
	"errors"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func fixture() *Checkpoint {
	return &Checkpoint{
		Schema: SchemaVersion, Generation: 1, CreatedAtMS: 1000,
		Identity: Identity{
			RecordID: "record", StorageID: "owner", MappingRevision: 1,
			LayoutVersion: 2, Root: filepath.FromSlash("payload/Film"),
			MetainfoDigest: strings.Repeat("a", 64), CatalogDigest: strings.Repeat("b", 64),
			VolumeID: "enrollment", PieceLength: 4, PieceCount: 3, ExtentLength: 12,
		},
		Files: []File{
			{Path: "a.bin", Offset: 0, Length: 3, Evidence: Evidence{Present: true, Identity: "file-a", Size: 3, ModifiedNS: 10}},
			{Path: "b.bin", Offset: 3, Length: 5, Evidence: Evidence{Present: true, Identity: "file-b", Size: 5, ModifiedNS: 20}},
			{Path: "c.bin", Offset: 8, Length: 4, Evidence: Evidence{Present: true, Identity: "file-c", Size: 4, ModifiedNS: 30}},
		},
		Pieces: []byte{0b111},
	}
}

func TestParseMode(t *testing.T) {
	for _, test := range []struct {
		raw  string
		want Mode
	}{{"", Fast}, {"fast", Fast}, {"verify", Verify}} {
		got, err := ParseMode(test.raw)
		if got != test.want || err != nil {
			t.Fatalf("ParseMode(%q) = %q, %v", test.raw, got, err)
		}
	}
	for _, raw := range []string{"managed", "FAST", " fast ", "disabled", "false"} {
		if mode, err := ParseMode(raw); err == nil || mode != "" {
			t.Fatalf("unknown mode accepted: %q", raw)
		}
	}
}

func TestCheckpointValidation(t *testing.T) {
	if err := fixture().Validate(); err != nil {
		t.Fatal(err)
	}
	var absent *Checkpoint
	if !errors.Is(absent.Validate(), ErrInvalid) {
		t.Fatal("nil checkpoint accepted")
	}
	tests := []struct {
		name   string
		change func(*Checkpoint)
		want   error
	}{
		{"schema", func(c *Checkpoint) { c.Schema++ }, ErrInvalid},
		{"generation", func(c *Checkpoint) { c.Generation = 0 }, ErrInvalid},
		{"time", func(c *Checkpoint) { c.CreatedAtMS = 0 }, ErrInvalid},
		{"record", func(c *Checkpoint) { c.Identity.RecordID = "" }, ErrInvalid},
		{"owner", func(c *Checkpoint) { c.Identity.StorageID = "" }, ErrInvalid},
		{"revision", func(c *Checkpoint) { c.Identity.MappingRevision = 0 }, ErrInvalid},
		{"volume", func(c *Checkpoint) { c.Identity.VolumeID = "" }, ErrInvalid},
		{"digest", func(c *Checkpoint) { c.Identity.MetainfoDigest = strings.Repeat("z", 64) }, ErrInvalid},
		{"layers_digest", func(c *Checkpoint) { c.Identity.CatalogDigest = "old" }, ErrInvalid},
		{"root_escape", func(c *Checkpoint) { c.Identity.Root = "../other" }, ErrInvalid},
		{"root_nul", func(c *Checkpoint) { c.Identity.Root += "\x00" }, ErrInvalid},
		{"layout", func(c *Checkpoint) { c.Identity.LayoutVersion = 99 }, ErrInvalid},
		{"piece_length", func(c *Checkpoint) { c.Identity.PieceLength = 0 }, ErrInvalid},
		{"count", func(c *Checkpoint) { c.Identity.PieceCount = 2 }, ErrInvalid},
		{"negative_count", func(c *Checkpoint) { c.Identity.PieceCount = -1 }, ErrInvalid},
		{"extent", func(c *Checkpoint) { c.Identity.ExtentLength = -1 }, ErrInvalid},
		{"pieces_budget", func(c *Checkpoint) { c.Identity.PieceCount = MaxPieces + 1 }, ErrBudget},
		{"bitmap_short", func(c *Checkpoint) { c.Pieces = nil }, ErrInvalid},
		{"bitmap_long", func(c *Checkpoint) { c.Pieces = []byte{7, 0} }, ErrInvalid},
		{"bitmap_tail", func(c *Checkpoint) { c.Pieces = []byte{0x87} }, ErrInvalid},
		{"no_files", func(c *Checkpoint) { c.Files = nil }, ErrInvalid},
		{"file_budget", func(c *Checkpoint) { c.Files = make([]File, MaxFiles+1) }, ErrBudget},
		{"file_escape", func(c *Checkpoint) { c.Files[0].Path = "../other" }, ErrInvalid},
		{"file_nul", func(c *Checkpoint) { c.Files[0].Path += "\x00" }, ErrInvalid},
		{"unclean_path", func(c *Checkpoint) { c.Files[0].Path = filepath.FromSlash("a/../other") }, ErrInvalid},
		{"file_collision", func(c *Checkpoint) { c.Files[1].Path = "A.BIN" }, ErrInvalid},
		{"file_parent", func(c *Checkpoint) { c.Files[1].Path = filepath.Join("a.bin", "child") }, ErrInvalid},
		{"file_overlap", func(c *Checkpoint) { c.Files[1].Offset = 2 }, ErrInvalid},
		{"negative_length", func(c *Checkpoint) { c.Files[0].Length = -1 }, ErrInvalid},
		{"overflow", func(c *Checkpoint) { c.Files[2].Length = math.MaxInt64 }, ErrInvalid},
		{"negative_size", func(c *Checkpoint) { c.Files[0].Evidence.Size = -1 }, ErrInvalid},
		{"unknown_file_id", func(c *Checkpoint) { c.Files[0].Evidence.Identity = "" }, ErrInvalid},
		{"file_id_budget", func(c *Checkpoint) { c.Files[0].Evidence.Identity = strings.Repeat("x", MaxIdentityLength+1) }, ErrBudget},
		{"path_budget", func(c *Checkpoint) { c.Files[0].Path = strings.Repeat("x", MaxPathLength+1) }, ErrBudget},
		{"depth_budget", func(c *Checkpoint) {
			c.Files[0].Path = strings.Repeat("d"+string(filepath.Separator), MaxPathDepth) + "file"
		}, ErrBudget},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := fixture()
			test.change(c)
			if err := c.Validate(); !errors.Is(err, test.want) {
				t.Fatalf("got %v, want %v", err, test.want)
			}
		})
	}
	t.Run("aggregate_path_budget", func(t *testing.T) {
		c := fixture()
		c.Files = make([]File, MaxFiles)
		for i := range c.Files {
			c.Files[i].Path = strconv.Itoa(i) + strings.Repeat("x", 300)
		}
		if err := c.Validate(); !errors.Is(err, ErrBudget) {
			t.Fatalf("aggregate paths not bounded: %v", err)
		}
	})
}

func TestFilterNarrowing(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Checkpoint, []File, []byte)
		want   byte
		reason Reason
	}{
		{"unchanged", func(*Checkpoint, []File, []byte) {}, 0b111, Restored},
		{"partial", func(c *Checkpoint, _ []File, _ []byte) { c.Pieces[0] = 0b101 }, 0b101, Restored},
		{"revoked_boundary", func(_ *Checkpoint, _ []File, r []byte) { r[0] = 0b010 }, 0b101, PiecesRevoked},
		{"already_incomplete", func(c *Checkpoint, _ []File, r []byte) { c.Pieces[0] = 0b101; r[0] = 0b010 }, 0b101, Restored},
		{"mtime_boundary", func(_ *Checkpoint, f []File, _ []byte) { f[1].Evidence.ModifiedNS++ }, 0b100, FilesChanged},
		{"replacement", func(_ *Checkpoint, f []File, _ []byte) { f[2].Evidence.Identity = "new-id" }, 0b011, FilesChanged},
		{"truncated", func(_ *Checkpoint, f []File, _ []byte) { f[0].Evidence.Size = 1 }, 0b110, FilesChanged},
		{"missing", func(_ *Checkpoint, f []File, _ []byte) { f[1].Evidence.Present = false }, 0b100, FilesChanged},
		{"missing_at_seal", func(c *Checkpoint, f []File, _ []byte) {
			c.Files[1].Evidence.Present = false
			f[1].Evidence.Present = false
		}, 0b100, FilesChanged},
		{"short_at_seal", func(c *Checkpoint, f []File, _ []byte) { c.Files[1].Evidence.Size = 1; f[1].Evidence.Size = 1 }, 0b100, FilesChanged},
		{"all_revoked", func(_ *Checkpoint, _ []File, r []byte) { r[0] = 7 }, 0, PiecesRevoked},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := fixture()
			current := append([]File(nil), c.Files...)
			revoked := []byte{0}
			test.change(c, current, revoked)
			before := bytes.Clone(c.Pieces)
			got := Filter(Fast, c, c.Identity, current, revoked, false)
			if got.Reason != test.reason || len(got.Bits) != 1 || got.Bits[0] != test.want {
				t.Fatalf("got %+v, want bits=%03b reason=%s", got, test.want, test.reason)
			}
			if !bytes.Equal(c.Pieces, before) {
				t.Fatal("filter mutated durable snapshot")
			}
			got.Bits[0] = 0xff
			if !bytes.Equal(c.Pieces, before) {
				t.Fatal("result aliases durable snapshot")
			}
		})
	}
}

func TestFilterFallback(t *testing.T) {
	c := fixture()
	tests := []struct {
		name     string
		mode     Mode
		saved    *Checkpoint
		expected Identity
		current  []File
		revoked  []byte
		blocked  bool
		reason   Reason
	}{
		{"verify", Verify, c, c.Identity, c.Files, []byte{0}, false, StrictMode},
		{"invalid_mode", "unknown", c, c.Identity, c.Files, []byte{0}, false, StrictMode},
		{"absent", Fast, nil, c.Identity, c.Files, []byte{0}, false, NoCheckpoint},
		{"journal_or_volume", Fast, c, c.Identity, c.Files, []byte{0}, true, StorageBlocked},
		{"missing_catalog", Fast, c, c.Identity, nil, []byte{0}, false, IdentityChanged},
		{"missing_revocations", Fast, c, c.Identity, c.Files, nil, false, InvalidCheckpoint},
		{"invalid_revocations", Fast, c, c.Identity, c.Files, []byte{0x80}, false, InvalidCheckpoint},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Filter(test.mode, test.saved, test.expected, test.current, test.revoked, test.blocked)
			if got.Reason != test.reason || len(got.Bits) != 0 {
				t.Fatalf("unsafe fallback: %+v", got)
			}
		})
	}
	for _, mutate := range []func(*Identity){
		func(i *Identity) { i.RecordID = "re-added" },
		func(i *Identity) { i.StorageID = "new-owner" },
		func(i *Identity) { i.MappingRevision++ },
		func(i *Identity) { i.Root = filepath.FromSlash("payload/Other") },
		func(i *Identity) { i.CatalogDigest = strings.Repeat("c", 64) },
		func(i *Identity) { i.VolumeID = "new-volume" },
	} {
		id := c.Identity
		mutate(&id)
		if got := Filter(Fast, c, id, c.Files, []byte{0}, false); got.Reason != IdentityChanged || len(got.Bits) != 0 {
			t.Fatalf("identity fence bypassed: %+v", got)
		}
	}
	current := append([]File(nil), c.Files...)
	current[0].Length++
	if got := Filter(Fast, c, c.Identity, current, []byte{0}, false); got.Reason != IdentityChanged || len(got.Bits) != 0 {
		t.Fatalf("catalog geometry changed: %+v", got)
	}
	c.Schema++
	if got := Filter(Fast, c, c.Identity, c.Files, []byte{0}, false); got.Reason != InvalidCheckpoint || len(got.Bits) != 0 {
		t.Fatalf("unknown schema trusted: %+v", got)
	}
	c = fixture()
	c.Identity.PieceCount = MaxPieces + 1
	if got := Filter(Fast, c, c.Identity, c.Files, []byte{0}, false); got.Reason != BudgetExceeded || len(got.Bits) != 0 {
		t.Fatalf("oversized checkpoint trusted: %+v", got)
	}
}

func TestV2AlignmentAndZeroLength(t *testing.T) {
	c := fixture()
	c.Files[0].Length = 2
	c.Files[0].Evidence.Size = 2
	c.Files[1].Offset = 4
	c.Files[1].Length = 4
	c.Files[1].Evidence.Size = 4
	c.Files = append(c.Files, File{Path: "empty", Offset: 12, Evidence: Evidence{}})
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	current := append([]File(nil), c.Files...)
	current[1].Evidence.ModifiedNS++
	got := Filter(Fast, c, c.Identity, current, []byte{0}, false)
	if len(got.Bits) != 1 || got.Bits[0] != 0b101 {
		t.Fatalf("v2 gap treated as shared v1 boundary: %+v", got)
	}
	// A bitmap has no evidence for empty files. Runtime must still open them.
	c.Identity.ExtentLength, c.Identity.PieceCount = 0, 0
	c.Files, c.Pieces = c.Files[3:], nil
	c.Files[0].Offset = 0
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	got = Filter(Fast, c, c.Identity, c.Files, nil, false)
	if len(got.Bits) != 0 {
		t.Fatal("empty file synthesized piece readiness")
	}
}

func TestGeometryOverflowAndLimits(t *testing.T) {
	c := fixture()
	c.Identity.PieceLength = math.MaxInt64
	c.Identity.ExtentLength = math.MaxInt64
	c.Identity.PieceCount = 1
	c.Pieces[0] = 1
	c.Files = []File{{Path: "huge", Length: math.MaxInt64, Evidence: Evidence{Present: true, Identity: "huge-id", Size: math.MaxInt64}}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	current := append([]File(nil), c.Files...)
	current[0].Evidence.ModifiedNS++
	if got := Filter(Fast, c, c.Identity, current, []byte{0}, false); len(got.Bits) != 1 || got.Bits[0] != 0 {
		t.Fatalf("overflow retained invalid piece: %+v", got)
	}
	c = fixture()
	c.Identity.PieceCount = MaxPieces
	c.Identity.ExtentLength = int64(MaxPieces) * c.Identity.PieceLength
	c.Pieces = make([]byte, bitmapBytes(MaxPieces))
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(c.Pieces) != 512<<10 {
		t.Fatal("bitmap budget changed")
	}
}

func FuzzFilterNeverAddsTrust(f *testing.F) {
	f.Add(byte(7), byte(0), byte(0))
	f.Add(byte(5), byte(2), byte(1))
	f.Fuzz(func(t *testing.T, pieces, revoked, changed byte) {
		c := fixture()
		c.Pieces[0] = pieces & 7
		current := append([]File(nil), c.Files...)
		for i := range current {
			if changed&(1<<uint(i)) != 0 {
				current[i].Evidence.ModifiedNS++
			}
		}
		r := revoked & 7
		got := Filter(Fast, c, c.Identity, current, []byte{r}, false)
		if len(got.Bits) != 1 || got.Bits[0]&^c.Pieces[0] != 0 || got.Bits[0]&r != 0 {
			t.Fatalf("trust increased: %+v", got)
		}
	})
}
