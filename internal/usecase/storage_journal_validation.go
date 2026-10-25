package usecase

import (
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	"torrent-stream-hub/internal/models"
	"torrent-stream-hub/internal/payload"
	"torrent-stream-hub/internal/repository"
)

// Validate before any filesystem access, including resume after database edits
// or an unsupported binary upgrade. Root containment alone does not establish
// ownership: a local ../ path can still address a different torrent.
func validateLocationJournal(v *repository.RecoveryRecord, j *storageLocationJournal) error {
	validPath := func(p string) bool {
		return payload.ValidateRelative(p) == nil && filepath.Clean(p) == p && (filepath.Separator == '\\' || !strings.Contains(p, `\`))
	}
	component := func(p string) bool { return validPath(p) && filepath.Base(p) == p }
	if j.Schema != 1 || j.Source == nil || !j.Source.Ready() || j.Source.Hash != v.Hash || j.Target.Validate() != nil || !component(v.RecordID) || !component(j.Source.StorageID) || v.Version < 1 || j.Source.Revision < 1 {
		return ErrStorageConflict
	}
	legacyStage := filepath.Join(".tsh-recovery", "moves", j.Source.StorageID, fmt.Sprintf("%s-%d", v.RecordID, v.Version))
	targetStage := filepath.Join(filepath.Dir(j.Target.Root), legacyStage)
	if (j.Staging != legacyStage && j.Staging != targetStage) ||
		(j.CleanupPath != "" && j.CleanupPath != filepath.Join(j.Source.Root, fmt.Sprintf(".tsh-cleanup-%s-%d", v.RecordID, v.Version))) ||
		payload.Overlap(payload.CollisionKey(j.Source.Root), payload.CollisionKey(j.Target.Root)) {
		return ErrStorageConflict
	}
	if len(j.SourceTree.Entries) > maxStorageManifestEntries || len(j.Entries) > maxStorageManifestEntries || len(j.Directories) > maxStorageManifestEntries || j.ExtraBytes < 0 {
		return ErrStorageConflict
	}
	if j.Method != "" && j.Method != "rename" && j.Method != "copy" && (v.Reason != models.StorageLocate || j.Method != "locate") {
		return ErrStorageConflict
	}
	tree := make(map[string]storageTreeEntry, len(j.SourceTree.Entries))
	bytes := 0
	for _, e := range j.SourceTree.Entries {
		bytes += len(e.Path)
		if !validPath(e.Path) || bytes > 16<<20 || e.Evidence.Identity == "" || e.Evidence.Size < 0 {
			return ErrStorageConflict
		}
		if _, exists := tree[e.Path]; exists {
			return ErrStorageConflict
		}
		if !e.Directory {
			if digest, err := hex.DecodeString(e.Evidence.Digest); err != nil || len(digest) != 32 {
				return ErrStorageConflict
			}
		}
		tree[e.Path] = e
	}
	seen := make(map[string]bool, len(j.Entries))
	if len(j.CandidateTree.Entries) > maxStorageManifestEntries {
		return ErrStorageConflict
	}
	for _, e := range j.CandidateTree.Entries {
		bytes += len(e.Path)
		if !validPath(e.Path) || bytes > 16<<20 || e.Evidence.Identity == "" || e.Evidence.Size < 0 {
			return ErrStorageConflict
		}
	}
	for _, e := range j.Entries {
		source, ok := tree[e.Source]
		expected, err := j.Target.Relative(filepath.Join(j.Source.Prefix, e.Source))
		if !ok || source.Directory || source.Evidence != e.Evidence || seen[e.Source] || !validPath(e.Target) || err != nil || expected != e.Target {
			return ErrStorageConflict
		}
		seen[e.Source] = true
	}
	for _, d := range j.Directories {
		if !validPath(d) {
			return ErrStorageConflict
		}
	}
	return nil
}
