package payload

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
)

func TestNamedLayoutPreservesCatalogAndStripsOnlyDirectory(t *testing.T) {
	for _, multi := range []bool{false, true} {
		info := &metainfo.Info{Name: "Series", Length: 3}
		if multi {
			info.Length = 0
			info.Files = []metainfo.FileInfo{{Length: 3, Path: []string{"episode.mkv"}}, {Length: 0, Path: []string{"empty.part"}}}
		}
		l := Layout{Version: NamedLayoutVersion, Root: filepath.FromSlash("payload/Series"), Prefix: MetadataPrefix(info)}
		files := info.UpvertedFiles()
		path, err := l.File(info, &files[0])
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.FromSlash("payload/Series/Series")
		if multi {
			want = filepath.FromSlash("payload/Series/episode.mkv")
		}
		if path != want {
			t.Fatalf("multi=%v: %q != %q", multi, path, want)
		}
		if Path(info, &files[0]) != map[bool]string{true: filepath.FromSlash("Series/episode.mkv"), false: "Series"}[multi] {
			t.Fatal("metainfo changed")
		}
	}
	l := Layout{Version: NamedLayoutVersion, Root: filepath.FromSlash("payload/Series"), Prefix: "Series"}
	for _, path := range []string{"../escape", "Other/episode", "Series/../escape", "Series"} {
		if _, err := l.Path(path); err == nil {
			t.Fatalf("accepted %q", path)
		}
	}
}

func TestFolderNamePortableAndStable(t *testing.T) {
	for input, want := range map[string]string{"CON": "Torrent_CON", "aux.txt": "Torrent_aux.txt", "../A:B?": "_A_B_", " . ": "Torrent", "Cafe\u0301": "Café", "film.part": "film.part"} {
		if got := FolderName(input); got != want {
			t.Fatalf("%q => %q, want %q", input, got, want)
		}
	}
	if len(FolderName(strings.Repeat("Я", 200))) > 180 {
		t.Fatal("too long")
	}
	if NamedRoot("Series", 2) != filepath.FromSlash("payload/Series (2)") {
		t.Fatal("collision name")
	}
}
