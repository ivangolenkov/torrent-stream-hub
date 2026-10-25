// Diagnostic projection only. Actions never bypass server-side safety gates.
export type StorageRecoveryReason =
  | 'legacy_file_conflict' | 'deletion_pending' | 'deletion_cancelled'
  | 'deletion_mode_conflict' | 'ownership_catalog_changed' | 'ownership_conflict'
  | 'migration_queued' | 'migration_active' | 'attachment_failed' | 'disk_full' | 'recovery_complete';

export type StorageRecoveryAction =
  | 'analyze_legacy' | 'apply_legacy' | 'retry_deletion' | 'cancel_deletion'
  | 'forget_record' | 'retry_ownership' | 'resume_verify';

export interface StorageProgress {
  stage: 'queued' | 'preflight' | 'checksum' | 'copy' | 'verification' | 'cleanup'
    | 'analysis' | 'preserve_alternative' | 'publication';
  queue_position?: number;
  bytes_done: number;
  bytes_total?: number;
  files_done: number;
  files_total?: number;
  file_path?: string;
  file_bytes_done: number;
  file_bytes_total?: number;
  stage_started_at_ms: number;
  last_progress_at_ms: number;
  heartbeat_at_ms: number;
}

export interface StorageRecovery {
  record_id: string;
  version: string;
  reason: StorageRecoveryReason;
  actions: StorageRecoveryAction[];
  runtime: 'absent' | 'attached' | 'failed';
  data_readiness: 'unknown' | 'checking' | 'verified';
  metadata_path?: string;
  failure_code?: string;
  repeat_count?: number;
  alternative_copies?: number;
  alternative_bytes?: number;
  analysis?: {
    path: string;
    equal: boolean;
    recommended?: 'final' | 'part';
    final: { size: number; valid_pieces: number; total_pieces: number };
    part: { size: number; valid_pieces: number; total_pieces: number };
    limitations: string[];
  };
  deletion?: {
    mode: 'keep' | 'payload';
    stage: 'intent' | 'runtime_stopped' | 'manifest_ready' | 'removing'
      | 'removing_record' | 'cancel_requested' | 'cancelled';
    removed_data: 'none' | 'partial' | 'unknown';
    error?: string;
  };
  progress?: StorageProgress;
}

export interface StorageActionError {
  error: string;
  code: StorageRecoveryReason;
  storage_recovery: StorageRecovery;
}
