package payload

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/anacrolix/torrent/metainfo"
	"golang.org/x/text/unicode/norm"
)

const NamedLayoutVersion = 2

func CollisionKey(path string) string { return strings.ToLower(norm.NFC.String(filepath.Clean(path))) }

// Layout is a persisted physical placement, independent of the metainfo
// catalog. Prefix is immutable within a mapping revision.
type Layout struct {
	Version int
	Root    string
	Prefix  string
}

func Supported(version int) bool { return version == LayoutVersion || version == NamedLayoutVersion }

func (l Layout) Validate() error {
	if !Supported(l.Version) || ValidateRelative(l.Root) != nil {
		return fmt.Errorf("unsupported storage layout")
	}
	if l.Version == LayoutVersion {
		if !ValidRoot(l.Root) || l.Prefix != "" {
			return fmt.Errorf("invalid legacy isolated root")
		}
	} else {
		if (filepath.Separator != '\\' && strings.Contains(l.Root, `\`)) || filepath.Clean(l.Root) != l.Root {
			return ErrUnsafePath
		}
		for _, p := range strings.Split(l.Root, string(filepath.Separator)) {
			if strings.HasPrefix(strings.ToLower(p), ".tsh-") {
				return ErrUnsafePath
			}
		}
		if l.Prefix != "" && (ValidateRelative(l.Prefix) != nil || strings.ContainsAny(l.Prefix, `/\`)) {
			return ErrUnsafePath
		}
	}
	return nil
}

func (l Layout) Relative(metadataPath string) (string, error) {
	if err := l.Validate(); err != nil {
		return "", err
	}
	if err := ValidateRelative(metadataPath); err != nil {
		return "", err
	}
	path := filepath.Clean(metadataPath)
	if l.Prefix != "" {
		var ok bool
		path, ok = strings.CutPrefix(path, l.Prefix+string(filepath.Separator))
		if !ok || ValidateRelative(path) != nil {
			return "", fmt.Errorf("metadata path outside persisted prefix")
		}
	}
	return path, nil
}

func (l Layout) Path(metadataPath string) (string, error) {
	path, err := l.Relative(metadataPath)
	if err != nil {
		return "", err
	}
	return filepath.Join(l.Root, path), nil
}

func (l Layout) File(info *metainfo.Info, file *metainfo.FileInfo) (string, error) {
	return l.Path(Path(info, file))
}

// MetadataPrefix removes only the known outer directory, never a filename.
func MetadataPrefix(info *metainfo.Info) string {
	name := info.BestName()
	if ValidateRelative(name) != nil || strings.ContainsAny(name, `/\`) {
		return ""
	}
	files := info.UpvertedFiles()
	if len(files) == 0 {
		return ""
	}
	for _, f := range files {
		path := Path(info, &f)
		if !strings.HasPrefix(path, name+string(filepath.Separator)) {
			return ""
		}
	}
	return name
}

// FolderName uses one portable naming policy on all platforms. Existing paths
// are never re-sanitized on read; the chosen name is stored once in SQLite.
func FolderName(name string) string {
	name = norm.NFC.String(strings.ToValidUTF8(name, "_"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.Trim(name, " .\t")
	if name == "" {
		name = "Torrent"
	}
	base := strings.ToUpper(strings.SplitN(name, ".", 2)[0])
	reserved := base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || base == "CONIN$" || base == "CONOUT$"
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '0' && base[3] <= '9' {
		reserved = true
	}
	if reserved || strings.HasPrefix(strings.ToLower(name), ".tsh-") {
		name = "Torrent_" + name
	}
	// Leave space for a collision suffix on filesystems with 255-byte names.
	for len(name) > 180 {
		_, n := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-n]
	}
	return strings.TrimRight(name, " .")
}

func NamedRoot(name string, ordinal int) string {
	name = FolderName(name)
	if ordinal > 1 {
		name = fmt.Sprintf("%s (%d)", name, ordinal)
	}
	return filepath.Join("payload", name)
}
