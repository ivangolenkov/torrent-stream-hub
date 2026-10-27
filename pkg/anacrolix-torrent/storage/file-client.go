package storage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/log"
	"golang.org/x/text/unicode/norm"

	"github.com/anacrolix/torrent/metainfo"
)

// File-based storage for torrents, that isn't yet bound to a particular torrent.
type fileClientImpl struct {
	opts   NewFileClientOpts
	closed atomic.Bool
}

// ErrStoragePending retains validated metadata while an application prepares
// its durable location outside the client lock. SetInfoBytes retries the open.
var ErrStoragePending = errors.New("storage preparation pending")

// All Torrent data stored in this baseDir. The info names of each torrent are used as directories.
func NewFile(baseDir string) ClientImplCloser {
	return NewFileWithCompletion(baseDir, pieceCompletionForDir(baseDir))
}

type NewFileClientOpts struct {
	// IOEvent observes payload storage syscall attempts and checkpoint boundaries
	// (snapshot/payload-sync/directory-sync before/after). After-sync means the
	// platform sync helper returned successfully, not proof of a syscall on every
	// platform or of hardware durability. It must be fast and
	// must not reenter this storage. Nil has no instrumentation overhead.
	IOEvent func(operation, path string)
	// The base directory for all downloads.
	ClientBaseDir       string
	FilePathMaker       FilePathMaker
	TorrentDirMaker     TorrentDirFilePathMaker
	TorrentRootResolver func(base string, info *metainfo.Info, hash metainfo.Hash) (string, error)
	// TorrentPathResolver returns one coherent root and physical relative path
	// per metadata file. It overrides both root and file makers for this open.
	TorrentPathResolver func(base string, info *metainfo.Info, hash metainfo.Hash) (string, []string, error)
	// If part files are enabled, this will default to inferring completion from file names at
	// startup, and keep the rest in memory.
	PieceCompletion PieceCompletion
	UsePartFiles    g.Option[bool]
	// ImportPartFiles moves legacy .part files to their metadata names before
	// opening storage. Requires RootFS, VerifyOnOpen and UsePartFiles=false.
	// Existing targets are never overwritten; conflicting names fail the open.
	ImportPartFiles bool
	Logger          *slog.Logger
	// RootFS contains all actual I/O. ClientBaseDir must name this same root.
	// Ownership transfers to the client and Close releases it.
	RootFS *os.Root
	// VerifyOnOpen ignores persistent completion and filename/size inference.
	VerifyOnOpen       bool
	NoCompletionRename bool
	IOContext          context.Context
	// ReadOnly is for offline recovery analysis: no zero-file creation, writes,
	// imports or completion renames. Completion must be in-memory.
	ReadOnly bool
	// IORead is an observational callback invoked after actual reads, outside
	// the torrent client lock. It must not perform filesystem/SQL I/O.
	IORead func(path string, n int64)
	// IOGuard checks the enrolled volume before rooted access/mutation. Paths
	// are relative to RootFS. It never grants piece completion.
	IOGuard func(string) error
	// ManagedPayload assumes no external changes while attached. Rooted opens
	// and volume checks run on descriptor acquisition, not on each chunk. Only
	// writable VerifyOnOpen storage without part files supports this mode.
	ManagedPayload bool
	// ResumeHooks is called once per open before namespace mutations. A nil
	// factory keeps the historical verified-only backend, never writable trust.
	ResumeHooks FileResumeFactory
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
	return &fileClientImpl{opts: opts}
}

func (me *fileClientImpl) Close() error {
	if me.closed.Swap(true) {
		return nil
	}
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
	if fs.closed.Load() {
		return TorrentImpl{}, os.ErrClosed
	}
	if fs.opts.ManagedPayload && (fs.opts.RootFS == nil || !fs.opts.VerifyOnOpen || fs.opts.ReadOnly || fs.opts.partFiles()) {
		return TorrentImpl{}, fmt.Errorf("managed payload requires writable rooted verified storage without part files")
	}
	if fs.opts.ReadOnly && (fs.opts.RootFS == nil || fs.opts.ImportPartFiles || fs.opts.partFiles() || !fs.opts.NoCompletionRename || pieceCompletionIsPersistent(fs.opts.PieceCompletion)) {
		return TorrentImpl{}, fmt.Errorf("read-only analysis requires rooted plain-name nonpersistent storage")
	}
	if fs.opts.ImportPartFiles && (fs.opts.RootFS == nil || !fs.opts.VerifyOnOpen || fs.opts.partFiles()) {
		return TorrentImpl{}, fmt.Errorf("importing part files requires rooted verified storage without part files")
	}
	dir := fs.opts.TorrentDirMaker(fs.opts.ClientBaseDir, info, infoHash)
	if fs.opts.TorrentRootResolver != nil {
		dir, err = fs.opts.TorrentRootResolver(fs.opts.ClientBaseDir, info, infoHash)
		if err != nil {
			return
		}
	}
	var resolvedPaths []string
	if fs.opts.TorrentPathResolver != nil {
		dir, resolvedPaths, err = fs.opts.TorrentPathResolver(fs.opts.ClientBaseDir, info, infoHash)
		if err != nil {
			return
		}
		if len(resolvedPaths) != len(info.UpvertedFiles()) {
			return TorrentImpl{}, fmt.Errorf("storage layout file count mismatch")
		}
	}
	logger := log.ContextLogger(ctx).Slogger()
	logger.DebugContext(ctx, "opened file torrent storage", slog.String("dir", dir))
	metainfoFileInfos := info.UpvertedFiles()
	files := make([]fileExtra, len(metainfoFileInfos))
	var normalized []string
	for i, fileInfo := range metainfoFileInfos {
		relative := fs.opts.FilePathMaker(FilePathMakerOpts{
			Info: info,
			File: &fileInfo,
		})
		if resolvedPaths != nil {
			relative = resolvedPaths[i]
		}
		if !filepath.IsLocal(relative) || relative == "." {
			return TorrentImpl{}, fmt.Errorf("unsafe mapped file path")
		}
		filePath := filepath.Join(dir, relative)
		if !isSubFilepath(dir, filePath) {
			err = fmt.Errorf("file %v: path %q is not sub path of %q", i, filePath, dir)
			return
		}
		files[i].safeOsPath = filePath
		files[i].index = i
		if fs.opts.RootFS != nil {
			normalized = append(normalized, strings.ToLower(norm.NFC.String(filePath)))
			if fs.opts.partFiles() {
				normalized = append(normalized, strings.ToLower(norm.NFC.String(filePath+".part")))
			}
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
	var resume *FileResumeHooks
	if fs.opts.ResumeHooks != nil {
		if fs.opts.RootFS == nil || !fs.opts.VerifyOnOpen || fs.opts.partFiles() {
			return TorrentImpl{}, ErrCheckpointUnsupported
		}
		resume, err = fs.opts.ResumeHooks(ctx, info, infoHash)
		if err != nil {
			return TorrentImpl{}, err
		}
		if resume != nil && (resume.Revoke == nil || resume.Invalidate == nil || !validCheckpointBits(resume.Pending, info.NumPieces())) {
			return TorrentImpl{}, ErrCheckpointUnsupported
		}
	}
	if fs.opts.ImportPartFiles {
		if fs.opts.IOGuard != nil {
			base := fs.opts.ClientBaseDir
			if fs.opts.RootFS != nil {
				base = fs.opts.RootFS.Name()
			}
			for i := range files {
				rel, e := filepath.Rel(base, files[i].safeOsPath)
				if e != nil {
					return TorrentImpl{}, e
				}
				if e = fs.opts.IOGuard(rel); e != nil {
					return TorrentImpl{}, e
				}
			}
		}
		if err := fs.importPartFilesWithBarrier(ctx, files, resume); err != nil {
			var conflict *LegacyPartConflictError
			if errors.As(err, &conflict) {
				relative, e := filepath.Rel(dir, conflict.Path)
				if e != nil {
					return TorrentImpl{}, e
				}
				return TorrentImpl{}, &LegacyPartConflictError{Path: relative}
			}
			return TorrentImpl{}, err
		}
	}
	for i := range metainfoFileInfos {
		filePath := files[i].safeOsPath
		if metainfoFileInfos[i].Length == 0 && !fs.opts.ReadOnly {
			if resume != nil && resume.RestoreCandidate {
				continue
			}
			if resume != nil {
				// Existing empty files require no mutation. Missing ones may only
				// be created after whole-owner durable namespace invalidation.
				_, probe := (rootFileIo{root: fs.opts.RootFS, readOnly: true}).stat(filePath)
				if probe == nil {
					continue
				}
				if !errors.Is(probe, os.ErrNotExist) {
					return TorrentImpl{}, probe
				}
				if err := resume.Invalidate(); err != nil {
					return TorrentImpl{}, err
				}
				clear(resume.Pending)
			}
			if fs.opts.RootFS != nil {
				var f fileWriter
				f, err = (rootFileIo{root: fs.opts.RootFS, ctx: fs.opts.IOContext, guard: fs.opts.IOGuard}).openForWrite(filePath, 0)
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
		directory:         dir,
		info:              info,
		files:             files,
		metainfoFileInfos: metainfoFileInfos,
		segmentLocater:    info.FileSegmentsIndex(),
		infoHash:          infoHash,
		io:                defaultFileIo(),
		client:            fs,
	}
	if resume != nil {
		t.resume.hooks = resume
		t.resume.pending = append([]byte(nil), resume.Pending...)
	}
	if fs.opts.RootFS != nil {
		t.io = rootFileIo{root: fs.opts.RootFS, ctx: fs.opts.IOContext, readOnly: fs.opts.ReadOnly, onRead: fs.opts.IORead, guard: t.ioGuard(), onEvent: fs.opts.IOEvent, timings: &t.ioTimings}
	}
	if fs.opts.VerifyOnOpen {
		t.fresh = make(map[int]pieceEvidence)
		t.hashSnapshot = make(map[int][]os.FileInfo)
		if fs.opts.RootFS != nil {
			for i := range metainfoFileInfos {
				if metainfoFileInfos[i].Length == 0 {
					t.refreshEmptyFile(i)
				}
			}
		}
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
		result.CachedRestored = func(index int) bool {
			t.freshMu.Lock()
			defer t.freshMu.Unlock()
			return !t.readClosed.Load() && t.fresh[index].restored && t.cachedCompletionLocked(index).Complete
		}
		result.ResumeObservation = func() ResumeObservation {
			if resume != nil {
				return resume.Observation
			}
			return ResumeObservation{}
		}
		result.CachedComplete = func(index int) bool { return t.cachedCompletion(index).Complete }
		result.RefreshFresh = t.refreshRange
		result.WithCheckpoint = t.withCheckpoint
		result.CheckpointState = t.checkpointState
		result.PrepareRestore = t.prepareRestore
		result.RestoreCheckpoint = t.restoreCheckpoint
		result.RevokeCheckpoint = t.revokeCheckpoint
		if t.managedPayload() {
			result.ResetManagedPayload = t.resetManagedPayload
		}
		if fs.opts.RootFS != nil {
			result.RefreshEmptyFile = t.refreshEmptyFile
			result.CachedEmptyFile = t.cachedEmptyFile
		}
	}
	if resume != nil && resume.RestoreCandidate && resume.Restore != nil {
		if err := resume.Restore(ctx, result); err != nil {
			return TorrentImpl{}, errors.Join(err, t.Close())
		}
	}
	if fs.opts.VerifyOnOpen && fs.opts.RootFS != nil {
		result.InitialIncomplete, err = t.initialIncomplete(ctx)
		if err != nil {
			return TorrentImpl{}, errors.Join(err, t.Close())
		}
	}
	result.IOTimings = func() IOTimings {
		s := t.ioTimings.snapshot()
		s.ManagedPayload = t.managedPayload()
		return s
	}
	return result, nil
}
