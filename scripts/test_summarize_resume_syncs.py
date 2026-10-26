import unittest

from summarize_resume_syncs import calls, summarize, windows


class ResumeSyncTraceTest(unittest.TestCase):
    def test_windows_and_interleaved_calls(self):
        markers = "\n".join(
            f"TSH_RESUME_MEASURE name=fixture stage={stage} unix_ns={ns}"
            for stage, ns in [("begin", 2_000_000_000), ("online-seal-begin", 3_000_000_000),
                              ("online-seal-end", 4_000_000_000), ("end", 5_000_000_000)])
        trace = """1 1.000000 fsync(3</x/hub.db-wal>) = 0
1 2.100000 fdatasync(3</x/hub.db-wal>) = 0
2 3.100000 fsync(4</x/payload/fixture-0.bin/fixture-0.bin> <unfinished ...>
1 3.200000 fsync(3</x/hub.db-wal>) = 0
2 3.300000 <... fsync resumed>) = 0
1 4.100000 fsync(4</x/payload>) = -1 EIO (Input/output error)
1 5.100000 fsync(3</x/hub.db>) = 0
"""
        result = summarize(trace, markers)[0]["counts"]
        self.assertEqual(result, {
            "startup.sqlite_wal.fdatasync.success": 1,
            "online_seal.payload.fsync.success": 1,
            "online_seal.sqlite_wal.fsync.success": 1,
            "shutdown.directory.fsync.failed": 1,
        })

    def test_partial_evidence_is_rejected(self):
        with self.assertRaises(ValueError):
            list(calls("1 1.000000 fsync(3</x> <unfinished ...>"))
        with self.assertRaises(ValueError):
            windows("TSH_RESUME_MEASURE name=x stage=begin unix_ns=1")
        with self.assertRaises(ValueError):
            list(calls("1 1.000000 fsync(3) = 0"))


if __name__ == "__main__":
    unittest.main()
