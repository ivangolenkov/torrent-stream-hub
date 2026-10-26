#!/usr/bin/env python3
"""Summarize Linux strace fsync/fdatasync calls inside resume benchmark windows.

Input: strace -f -yy -ttt -e trace=fsync,fdatasync; benchmark environment
TSH_RESUME_TRACE=1. Fixture setup/teardown outside begin/end is excluded. Counts
are observed syscall attempts/successes, not hardware durability acknowledgments.
"""

import argparse
from collections import Counter
from decimal import Decimal
import json
import re


def windows(text):
    out, current = [], None
    for name, stage, ns in re.findall(
        r"TSH_RESUME_MEASURE name=(\S+) stage=(\S+) unix_ns=(\d+)", text
    ):
        if stage == "begin":
            if current is not None:
                raise ValueError("overlapping benchmark windows")
            current = {"name": name, "times": {}}
        if current is None or current["name"] != name or stage in current["times"]:
            raise ValueError("unmatched benchmark marker")
        current["times"][stage] = int(ns)
        if stage == "end":
            stages = ("begin", "online-seal-begin", "online-seal-end", "end")
            times = [current["times"][s] for s in stages]
            if times != sorted(times):
                raise ValueError("nonmonotonic benchmark markers")
            out.append(current)
            current = None
    if current is not None or not out:
        raise ValueError("incomplete or absent benchmark windows")
    return out


def calls(text):
    pending = {}
    for line in text.splitlines():
        match = re.match(r"(\d+)\s+(\d+\.\d+)\s+(.*)", line)
        if not match:
            continue
        pid, stamp, body = match.groups()
        ns = int(Decimal(stamp) * 1_000_000_000)
        if "resumed>" in body:
            if pid not in pending:
                raise ValueError("resumed sync without entry")
            ns, prefix = pending.pop(pid)
            body = prefix + body.split("resumed>", 1)[1]
        elif "<unfinished ...>" in body and re.match(r"(?:fsync|fdatasync)\(", body):
            if pid in pending:
                raise ValueError("two unfinished calls on one thread")
            pending[pid] = (ns, body.split("<unfinished ...>", 1)[0])
            continue
        sync = re.match(r"(fsync|fdatasync)\(\d+<([^>]+)>\s*\)\s*=\s*(-?\d+)", body)
        if sync:
            operation, path, result = sync.groups()
            yield ns, operation, path, int(result) == 0
        elif re.match(r"(?:fsync|fdatasync)\(", body):
            raise ValueError("undecoded sync path/result; use strace -f -yy -ttt")
    if pending:
        raise ValueError("unfinished sync at trace end")


def category(path):
    if path.endswith(".db-wal"):
        return "sqlite_wal"
    if path.endswith(".db") or path.endswith(".db-journal"):
        return "sqlite_db_or_journal"
    # This classifier is deliberately scoped to BenchmarkFastResume's named
    # single-file fixtures; fail visibly into 'other', never guess all .bin paths.
    if re.search(r"/payload/fixture-\d+\.bin/fixture-\d+\.bin$", path):
        return "payload"
    if re.search(r"/payload(?:/fixture-\d+\.bin)?$", path) or re.search(r"/\d+$", path):
        return "directory"
    return "other"


def summarize(trace, benchmark):
    runs = windows(benchmark)
    totals = [Counter() for _ in runs]
    decoded = 0
    for ns, operation, path, success in calls(trace):
        decoded += 1
        for i, run in enumerate(runs):
            time = run["times"]
            if not time["begin"] <= ns < time["end"]:
                continue
            phase = "startup"
            if ns >= time["online-seal-end"]:
                phase = "shutdown"
            elif ns >= time["online-seal-begin"]:
                phase = "online_seal"
            suffix = "success" if success else "failed"
            totals[i][f"{phase}.{category(path)}.{operation}.{suffix}"] += 1
    if not decoded:
        raise ValueError("no sync syscalls decoded")
    return [{"name": run["name"], "counts": dict(sorted(count.items()))}
            for run, count in zip(runs, totals)]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("trace")
    parser.add_argument("benchmark")
    args = parser.parse_args()
    with open(args.trace) as trace, open(args.benchmark) as benchmark:
        print(json.dumps(summarize(trace.read(), benchmark.read()), indent=2))


if __name__ == "__main__":
    main()
