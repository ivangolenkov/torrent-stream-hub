# Fast resume durability measurements

Use the same fixed fixtures for both policies (4 × 2 MiB, 64 KiB pieces):

```sh
go test ./internal/usecase -run '^$' \
  -bench 'BenchmarkFastResume(Baseline|Result)' -benchtime=1x -count=3 -timeout=180s
```

`Baseline` pins `verify`; `Result` uses `fast`. Setup is excluded from the custom
latencies. Complete and mixed (two complete, two half-valid) cases are separate.
The optional 1 ms read delay emulates contention, not NAS throughput.

In addition to hashing/API/Range/memory metrics:

- `*-write-tx` counts **committed write transactions** using SQLite WAL commit
  frames. Empty/read transactions and rolled-back writes are not counted.
- `WAL-appended-B` is append-only WAL growth; autocheckpoint is disabled on every
  measurement connection. Writer admission is reserved while reading the WAL to
  avoid racing the disposable-hint writer. Salt changes/partial frames fail the
  measurement rather than producing guessed counts.
- `online-seal-4-owners-ms` explicitly seals all four owners through the production
  source/adapter with command reservations without dropping runtime. Reader/write
  continuity is covered separately by the installed periodic integration test. It
  measures service time, **not** the 60-second scheduling delay. It can force an
  unchanged owner to seal and may overlap previously queued work.
- `checkpoint-*-syncs` counts successful platform sync-helper returns, **not**
  physical fsync operations or hardware acknowledgments on every platform.
- SQL trigger row counts, WAL transactions and OS sync calls are different facts.

## Actual Linux sync syscalls

Build a Linux test binary and run it on a native Linux kernel with `strace`
installed. For Linux arm64 on an arm64 host:

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -c ./internal/usecase -o usecase-linux.test
TSH_RESUME_TRACE=1 strace -f -yy -ttt -e trace=fsync,fdatasync \
  -o resume-sync.trace ./usecase-linux.test -test.run '^$' \
  -test.bench 'BenchmarkFastResume(Baseline|Result)' -test.benchtime=1x \
  -test.count=3 -test.timeout=180s > resume-benchmark.log 2>&1
python3 scripts/summarize_resume_syncs.py resume-sync.trace resume-benchmark.log
python3 -m unittest discover -s scripts -p test_summarize_resume_syncs.py
```

Docker's Linux VM is suitable for a local-filesystem Linux execution, not a
Windows/NAS/power-loss test. A restricted container may need `SYS_PTRACE` for its
own child tracer. Do not attach the tracer to unrelated services.

The parser pairs interleaved unfinished/resumed syscalls, keeps failed calls
separate, and uses benchmark wall-clock markers to exclude fixture setup and
cleanup. It classifies **these named single-file benchmark fixtures**, not
arbitrary application paths. `startup` ends at the explicit online seal;
`shutdown` starts when that seal returns. Engine shutdown is included, but the
subsequent external SQLite DB close/final WAL checkpoint is outside the window.

Tracing changes scheduling and can admit extra background/queued checkpoints.
Do not compare traced latency to untraced latency or infer syscall counts from
another OS's WAL counters. Counts are OS attempts/successes, not evidence that
hardware honored flush. No MB/s, latency or fsync-count CI thresholds are set.
