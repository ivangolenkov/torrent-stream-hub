package storage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/log"
	"golang.org/x/text/unicode/norm"

	"github.com/anacrolix/torrent/metainfo"
)

// File-based storage for torrents, that isn't yet bound to a particular torrent.
type fileClientImpl struct {
	opts NewFileClientOpts
}

// All Torrent data stored in this baseDir. The info names of each torrent are used as directories.
func NewFile(baseDir string) ClientImplCloser {
	return NewFileWithCompletion(baseDir, pieceCompletionForDir(baseDir))
}

type NewFileClientOpts struct {
	// The base directory for all downloads.
	ClientBaseDir       string
	FilePathMaker       FilePathMaker
	TorrentDirMaker     TorrentDirFilePathMaker
	TorrentRootResolver func(base string, info *metainfo.Info, hash metainfo.Hash) (string, error)
	// If part files are enabled, this will default to inferring completion from file names at
	// startup, and keep the rest in memory.
	PieceCompletion PieceCompletion
	UsePartFiles    g.Option[bool]
	Logger          *slog.Logger
	// RootFS contains all actual I/O. ClientBaseDir must name this same root.
	// Ownership transfers to the client and Close releases it.
	RootFS *os.Root
	// VerifyOnOpen ignores persistent completion and filename/size inference.
	VerifyOnOpen       bool
	NoCompletionRename bool
	IOContext          context.Context
}

// The specific part-files option or the default.
func (me NewFileClientOpts) partFiles() bool {
	return me.UsePartFiles.UnwrapOr(true)
}

// NewFileOpts creates a new ClientImplCloser that stores files using the OS native filesystem.
func NewFileOpts(opts NewFileClientOpts) ClientImplCloser {
	if opts.TorrentDirMaker == nil {
		opts.TorrentDirMaker = defaultPathMaker
	}
	if opts.FilePathMaker == nil {
		opts.FilePathMaker = DefaultFilePathMaker
	}
	if opts.PieceCompletion == nil {
		if opts.partFiles() {
			opts.PieceCompletion = NewMapPieceCompletion()
		} else {
			opts.PieceCompletion = pieceCompletionForDir(opts.ClientBaseDir)
		}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &fileClientImpl{opts}
}

func (me *fileClientImpl) Close() error {
	err := me.opts.PieceCompletion.Close()
	if me.opts.RootFS != nil {
		err = errors.Join(err, me.opts.RootFS.Close())
	}
	return err
}

var defaultFileIo = func() fileIo {
	return classicFileIo{}
}

func (fs *fileClientImpl) OpenTorrent(
	ctx context.Context,
	info *metainfo.Info,
	infoHash metainfo.Hash,
) (_ TorrentImpl, err error) {
	dir := fs.opts.TorrentDirMaker(fs.opts.ClientBaseDir, info, infoHash)
	if fs.opts.TorrentRootResolver != nil {
		dir, err = fs.opts.TorrentRootResolver(fs.opts.ClientBaseDir, info, infoHash)
		if err != nil {
			return
		}
	}
	logger := log.ContextLogger(ctx).Slogger()
	logger.DebugContext(ctx, "opened file torrent storage", slog.String("dir", dir))
	metainfoFileInfos := info.UpvertedFiles()
	files := make([]fileExtra, len(metainfoFileInfos))
	var normalized []string
	for i, fileInfo := range metainfoFileInfos {
		filePath := filepath.Join(dir, fs.opts.FilePathMaker(FilePathMakerOpts{
			Info: info,
			File: &fileInfo,
		}))
		if !isSubFilepath(dir, filePath) {
			err = fmt.Errorf("file %v: path %q is not sub path of %q", i, filePath, dir)
			return
		}
		files[i].safeOsPath = filePath
		if fs.opts.RootFS != nil {
			normalized = append(normalized, strings.ToLower(norm.NFC.String(filePath)), strings.ToLower(norm.NFC.String(filePath+".part")))
		}
	}
	if fs.opts.RootFS != nil {
		seen := make(map[string]bool, len(normalized))
		for _, path := range normalized {
			if seen[path] {
				return TorrentImpl{}, fmt.Errorf("ambiguous payload paths")
			}
			seen[path] = true
		}
		for _, path := range normalized {
			for parent := filepath.Dir(path); parent != "."; parent = filepath.Dir(parent) {
				if seen[parent] {
					return TorrentImpl{}, fmt.Errorf("payload file overlaps parent directory")
				}
				if filepath.Dir(parent) == parent {
					break
				}
			}
		}
	}
	for i := range metainfoFileInfos {
		filePath := files[i].safeOsPath
		if metainfoFileInfos[i].Length == 0 {
			if fs.opts.RootFS != nil {
				var f fileWriter
				f, err = (rootFileIo{root: fs.opts.RootFS, ctx: fs.opts.IOContext}).openForWrite(filePath, 0)
				if err == nil {
					err = f.Close()
				}
			} else {
				err = CreateNativeZeroLengthFile(filePath)
			}
			if err != nil {
				err = fmt.Errorf("creating zero length file: %w", err)
				return
			}
		}
	}
	t := &fileTorrentImpl{
		info:              info,
		files:             files,
		metainfoFileInfos: metainfoFileInfos,
		segmentLocater:    info.FileSegmentsIndex(),
		infoHash:          infoHash,
		io:                defaultFileIo(),
		client:            fs,
	}
	if fs.opts.RootFS != nil {
		t.io = rootFileIo{root: fs.opts.RootFS, ctx: fs.opts.IOContext}
	}
	if fs.opts.VerifyOnOpen {
		t.fresh = make(map[int][]os.FileInfo)
		t.hashSnapshot = make(map[int][]os.FileInfo)
	}
	if t.partFiles() && !fs.opts.VerifyOnOpen {
		err = t.setCompletionFromPartFiles()
		if err != nil {
			err = fmt.Errorf("setting completion from part files: %w", err)
			return
		}
	}
	result := TorrentImpl{
		Piece: t.Piece,
		Close: t.Close,
	}
	if fs.opts.VerifyOnOpen {
		result.FreshComplete = func(index int) bool { return t.freshCompletion(index).Complete }
	}
	return result, nil
}
