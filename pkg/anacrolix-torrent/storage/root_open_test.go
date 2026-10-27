package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestRootWriteOpenSubstageBarriers(t *testing.T) {
	for _, stage := range []string{"guard", "path", "mkdir", "descriptor", "identity"} {
		t.Run(stage, func(t *testing.T) {
			ts, p, _ := writeConcurrencyFixture(t)
			if stage == "mkdir" {
				if err := os.Remove(p.t.files[0].safeOsPath); err != nil {
					t.Fatal(err)
				}
			}
			r := p.t.io.(rootFileIo)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			stop := func(which string) {
				if which != stage {
					return
				}
				selected := false
				once.Do(func() { selected = true; close(entered) })
				if selected {
					<-release
				}
			}
			r.guard = func(string) error { stop("guard"); return nil }
			r.onEvent = func(op, _ string) {
				switch op {
				case "lstat":
					stop("path")
				case "mkdir":
					stop("mkdir")
				case "open":
					stop("descriptor")
				case "fstat":
					stop("identity")
				}
			}
			p.t.io = r
			done := make(chan error, 1)
			go func() { _, err := p.WriteAt([]byte("GOOD"), 0); done <- err }()
			joined := false
			defer func() {
				if !joined {
					close(release)
					<-done
				}
			}()
			awaitWriteBarrier(t, entered)
			s := ts.IOTimings()
			active := map[string]IOStageTimings{"guard": s.WriteOpenGuard, "path": s.WriteOpenPath, "mkdir": s.WriteOpenMkdir, "descriptor": s.WriteOpenDescriptor, "identity": s.WriteOpenIdentity}[stage]
			if active.Active != 1 || s.WriteOpen.Active != 1 || s.WriteLeaseHold.Active != 1 {
				t.Fatal("blocked open substage not observable", stage, s)
			}
			if p.t.files[0].payloadMu.TryLock() {
				p.t.files[0].payloadMu.Unlock()
				t.Fatal("open substage escaped mutation exclusion")
			}
			if ts.IOTimings() != s {
				t.Fatal("polling changed open-stage counters")
			}
			close(release)
			err := <-done
			joined = true
			if err != nil {
				t.Fatal(err)
			}
			s = ts.IOTimings()
			for _, done := range []IOStageTimings{s.WriteOpenGuard, s.WriteOpenPath, s.WriteOpenMkdir, s.WriteOpenDescriptor, s.WriteOpenIdentity} {
				if done.Active != 0 || done.Errors != 0 || done.NS < 0 || done.MaxNS > done.NS {
					t.Fatal(done)
				}
			}
		})
	}
}

func rootOpenFixture(t testing.TB) (string, *os.Root, string) {
	t.Helper()
	dir := t.TempDir()
	name := filepath.Join("a", "b", "data")
	if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("good"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return dir, root, name
}

func TestRootWriteOpenProbeBudget(t *testing.T) {
	dir, root, name := rootOpenFixture(t)
	var timings ioTimings
	events := make(map[string]int)
	guards := 0
	r := rootFileIo{root: root, timings: &timings,
		guard: func(rel string) error {
			guards++
			if rel != name {
				t.Errorf("unexpected relative path %q", rel)
			}
			return nil
		},
		onEvent: func(op, _ string) { events[op]++ },
	}
	for i := 0; i < 3; i++ {
		f, err := r.openForWrite(filepath.Join(dir, name), 4)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	// One three-component pathwalk plus the pre-open identity snapshot.
	// Previously: two pathwalks plus snapshot, and MkdirAll on every chunk.
	if events["lstat"] != 12 || events["mkdir"] != 0 || events["open"] != 3 || events["fstat"] != 3 || guards != 3 {
		t.Fatal(events, guards)
	}
	s := timings.snapshot()
	for _, stage := range []IOStageTimings{s.WriteOpenGuard, s.WriteOpenPath, s.WriteOpenDescriptor, s.WriteOpenIdentity} {
		if stage.Count != 3 || stage.Active != 0 || stage.Errors != 0 || stage.NS < 0 || stage.MaxNS > stage.NS {
			t.Fatal(stage)
		}
	}
	if s.WriteOpenMkdir != (IOStageTimings{}) {
		t.Fatal("existing file caused mkdir", s.WriteOpenMkdir)
	}
	before := timings.snapshot()
	f, err := r.openForRead(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if timings.snapshot() != before {
		t.Fatal("read polluted writer-open stages")
	}
}

func TestRootWriteOpenCreatesOnlyMissingParents(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var timings ioTimings
	r := rootFileIo{root: root, timings: &timings}
	name := filepath.Join(dir, "new", "nested", "data")
	for i := 0; i < 2; i++ {
		f, err := r.openForWrite(name, 4)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteAt([]byte("good"), 0); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	s := timings.snapshot()
	if s.WriteOpenMkdir.Count != 1 || s.WriteOpenMkdir.Errors != 0 || s.WriteOpenPath.Errors != 0 {
		t.Fatal(s)
	}
}

func TestRootOpenExclusiveCreateSemantics(t *testing.T) {
	dir, root, name := rootOpenFixture(t)
	f, err := OpenRootFile(root, name, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if f != nil {
		f.Close()
	}
	if !errors.Is(err, os.ErrExist) {
		t.Fatal("exclusive create overwrote existing entry", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil || string(data) != "good" {
		t.Fatal(string(data), err)
	}
	f, err = OpenRootFile(root, filepath.Join("a", "b", "new"), os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// Test-only reconstruction of the pre-optimization EXISTING-file path, not a
// production fallback. Keep the duplicate walk and unconditional MkdirAll to
// quantify redundant probes without relying on filesystem speed thresholds.
func legacyExistingRootWriteOpen(r rootFileIo, name string) (fileWriter, error) {
	rel, err := r.relative(name)
	if err != nil {
		return nil, err
	}
	if r.guard != nil {
		if err := r.guard(rel); err != nil {
			return nil, err
		}
	}
	rel, err = r.relative(filepath.Join(r.root.Name(), rel))
	if err != nil {
		return nil, err
	}
	r.event("lstat", rel)
	expected, err := r.root.Lstat(rel)
	if err != nil {
		return nil, err
	}
	r.event("mkdir", filepath.Dir(rel))
	if err := r.root.MkdirAll(filepath.Dir(rel), dirPerm); err != nil {
		return nil, err
	}
	r.event("open", rel)
	f, err := r.root.OpenFile(rel, os.O_WRONLY|os.O_CREATE, filePerm)
	if err != nil {
		return nil, err
	}
	r.event("fstat", rel)
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || FileHasMultipleLinks(f, st) || !os.SameFile(expected, st) {
		f.Close()
		return nil, ErrUnsafeFile
	}
	return f, nil
}

func TestRootOpenProbeBudgetComparison(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		name := "optimized"
		if legacy {
			name = "legacy-existing-path"
		}
		t.Run(name, func(t *testing.T) {
			dir, root, path := rootOpenFixture(t)
			events := make(map[string]int)
			guards := 0
			r := rootFileIo{root: root, guard: func(string) error { guards++; return nil }, onEvent: func(op, _ string) { events[op]++ }}
			open := r.openForWrite
			if legacy {
				open = func(name string, _ int64) (fileWriter, error) { return legacyExistingRootWriteOpen(r, name) }
			}
			f, err := open(filepath.Join(dir, path), 4)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			wantStats, wantMkdir := 4, 0
			if legacy {
				wantStats, wantMkdir = 7, 1
			}
			if events["lstat"] != wantStats || events["mkdir"] != wantMkdir || guards != 1 {
				t.Fatal(events, guards)
			}
		})
	}
}

func BenchmarkRootWriteOpenExisting(b *testing.B) {
	for _, legacy := range []bool{true, false} {
		name := "optimized"
		if legacy {
			name = "legacy-existing-path"
		}
		b.Run(name, func(b *testing.B) {
			dir, root, path := rootOpenFixture(b)
			path = filepath.Join(dir, path)
			var timings ioTimings
			events := make(map[string]int)
			r := rootFileIo{root: root, timings: &timings, guard: func(string) error { return nil }, onEvent: func(op, _ string) { events[op]++ }}
			open := r.openForWrite
			if legacy {
				open = func(name string, _ int64) (fileWriter, error) { return legacyExistingRootWriteOpen(r, name) }
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				f, err := open(path, 4)
				if err != nil {
					b.Fatal(err)
				}
				if err := f.Close(); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(events["lstat"])/float64(b.N), "lstat-attempts/op")
			b.ReportMetric(float64(events["mkdir"])/float64(b.N), "mkdir-calls/op")
		})
	}
}

func TestRootWriteOpenGuardStillFencesMutation(t *testing.T) {
	for _, mode := range []string{"guard-error", "cancel-in-guard", "guard-installs-symlink", "outside-root", "read-only", "replace-at-open", "remove-at-open"} {
		t.Run(mode, func(t *testing.T) {
			dir, root, name := rootOpenFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var timings ioTimings
			injected := errors.New("volume unavailable")
			r := rootFileIo{root: root, timings: &timings, ctx: ctx}
			guardCalls := 0
			r.guard = func(string) error {
				guardCalls++
				switch mode {
				case "guard-error":
					return injected
				case "cancel-in-guard":
					cancel()
				case "guard-installs-symlink":
					if err := os.Symlink("data", filepath.Join(dir, "a", "b", "link")); err != nil {
						t.Skipf("symlink unavailable: %v", err)
					}
				}
				return nil
			}
			if mode == "guard-installs-symlink" {
				name = filepath.Join("a", "b", "link")
			}
			if mode == "outside-root" {
				name = filepath.Join("..", "outside")
			}
			if mode == "read-only" {
				r.readOnly = true
			}
			if mode == "replace-at-open" || mode == "remove-at-open" {
				r.onEvent = func(op, _ string) {
					if op != "open" {
						return
					}
					if err := os.Rename(filepath.Join(dir, name), filepath.Join(dir, "old")); err != nil {
						t.Fatal(err)
					}
					if mode == "remove-at-open" {
						return
					}
					if err := os.WriteFile(filepath.Join(dir, name), []byte("evil"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			f, err := r.openForWrite(filepath.Join(dir, name), 4)
			if f != nil {
				f.Close()
			}
			if err == nil {
				t.Fatal("unsafe open accepted", mode)
			}
			s := timings.snapshot()
			if mode == "guard-error" && (!errors.Is(err, injected) || s.WriteOpenGuard.Errors != 1 || s.WriteOpenPath.Count != 0) {
				t.Fatal(err, s)
			}
			if mode == "cancel-in-guard" && (!errors.Is(err, context.Canceled) || s.WriteOpenDescriptor.Count != 0) {
				t.Fatal(err, s)
			}
			if (mode == "outside-root" || mode == "read-only") && guardCalls != 0 {
				t.Fatal("invalid access reached volume guard")
			}
			if mode == "guard-installs-symlink" && s.WriteOpenPath.Errors != 1 {
				t.Fatal(s)
			}
			if mode == "replace-at-open" && (!errors.Is(err, ErrUnsafeFile) || s.WriteOpenIdentity.Errors != 1) {
				t.Fatal(err, s)
			}
			if mode == "remove-at-open" {
				if !errors.Is(err, os.ErrNotExist) || s.WriteOpenDescriptor.Errors != 1 {
					t.Fatal(err, s)
				}
				if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("open recreated disappeared payload", err)
				}
			}
		})
	}
}
