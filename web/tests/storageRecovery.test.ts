import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import type { StorageActionError, StorageRecovery, StorageRecoveryAction, StorageRecoveryReason } from '../src/types/storageRecovery';

// The same fixture is round-tripped by Go, including strict field names and
// omission semantics. Typed literals here independently pin the TS contract.
const statuses = JSON.parse(readFileSync(resolve(process.cwd(), '../internal/models/testdata/storage-recovery.json'), 'utf8')) as StorageRecovery[];

describe('storage recovery wire contract', () => {
  it('pins analysis, retained alternatives and typed failure diagnostics', () => {
    const expected: StorageRecovery = { record_id: 'record-a', version: '11', reason: 'legacy_file_conflict',
      actions: ['apply_legacy'], runtime: 'failed', data_readiness: 'unknown', metadata_path: 'series/episode.mkv',
      analysis: { path: 'series/episode.mkv', equal: false, recommended: 'part',
        final: { size: 8, valid_pieces: 0, total_pieces: 1 }, part: { size: 8, valid_pieces: 1, total_pieces: 1 },
        limitations: ['Analysis does not grant runtime readiness.'] },
      alternative_copies: 1, alternative_bytes: 8, failure_code: 'flush_failed', repeat_count: 3 };
    expect(statuses[10]).toEqual(expected);
  });
  it('retains the legacy error field in action errors', () => {
    const response: StorageActionError = { error: 'deletion mode differs', code: 'deletion_mode_conflict',
      storage_recovery: { record_id: 'record-a', version: '4', reason: 'deletion_mode_conflict',
        actions: ['retry_deletion', 'cancel_deletion'], runtime: 'absent', data_readiness: 'unknown' } };
    expect(JSON.stringify(response)).toBe('{"error":"deletion mode differs","code":"deletion_mode_conflict","storage_recovery":{"record_id":"record-a","version":"4","reason":"deletion_mode_conflict","actions":["retry_deletion","cancel_deletion"],"runtime":"absent","data_readiness":"unknown"}}');
  });
  it('uses stable reason and action codes, not Go error text', () => {
    const reasons: StorageRecoveryReason[] = ['legacy_file_conflict', 'deletion_pending', 'deletion_cancelled', 'deletion_mode_conflict', 'ownership_catalog_changed', 'migration_queued', 'migration_active', 'attachment_failed', 'disk_full'];
    expect(statuses.slice(0, 9).map(s => s.reason)).toEqual(reasons);
    const actions: StorageRecoveryAction[] = ['analyze_legacy', 'retry_deletion', 'cancel_deletion', 'forget_record', 'resume_verify', 'retry_ownership', 'apply_legacy'];
    expect([...new Set(statuses.flatMap(s => s.actions))].sort()).toEqual(actions.sort());
    expect(statuses[0]?.version).toBe('9007199254740993');
  });

  it('pins the complete typed projection and independent progress clocks', () => {
    const expected: StorageRecovery = {
      record_id: 'record-a', version: '7', reason: 'migration_active', actions: [],
      runtime: 'absent', data_readiness: 'checking',
      progress: { stage: 'copy', bytes_done: 131072, bytes_total: 1073741824,
        files_done: 0, files_total: 1, file_path: 'series/episode.mkv',
        file_bytes_done: 131072, file_bytes_total: 1073741824,
        stage_started_at_ms: 1000, last_progress_at_ms: 2000, heartbeat_at_ms: 7000 },
    };
    expect(statuses[6]).toEqual(expected);
    expect(statuses[5]?.progress).not.toHaveProperty('bytes_total');
    expect(statuses[9]?.progress?.bytes_total).toBe(0);
    expect(statuses[2]?.deletion?.removed_data).toBe('unknown');
    expect(statuses[2]?.actions).not.toContain('retry_deletion');
  });
});
