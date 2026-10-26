export interface StorageLocation {
 inventory?: string;
 inventory_reason?: string;
 inventory_observed_at_ms?: number;
  id: string;
  revision: string;
  layout_version: number;
  root: string;
  prefix: string;
  status: string;
  reason?: string;
  operation?: {
    analysis?: { found_files: number; missing_files: number; valid_pieces: number; total_pieces: number; limitations: string[] };
    kind: 'move' | 'locate'; stage: string; version: string;
    source: string; target: string; actions: string[];
    method?: string; extra_bytes?: number; free_bytes?: number; error?: string; failure_code?: string;
  };
}
export interface FileStatus {
	read_reason?: string;
	last_known_at_ms?: number;
	last_known_source?: string;
	checked_bytes?: number;
	skipped_missing_bytes?: number;
	verification_total?: number;
  storage_path: string;
  presence: 'unknown' | 'present' | 'missing' | 'unavailable';
  reason?: string;
  verified_bytes: number;
  last_known_bytes: number;
  observed_at_ms?: number;
  verification: 'unknown' | 'queued' | 'running' | 'completed' | 'failed' | 'cancelled';
  direct_read_ready: boolean;
  effective_priority: string;
}
export interface SelectionProgress {
	catalog_version?: string; revision?: string; complete?: boolean;
  size: number; verified_bytes: number; progress?: number; files: number;
}
export interface VerificationProgress {
  progress_at_ms?: number;
  attachment?: string; operation?: number; status: string;
  checked_bytes: number; total_bytes: number; queued_pieces: number; active_pieces: number;
  // Startup negative evidence, not a current file presence/readiness signal.
  skipped_missing_bytes?: number;
}
export interface StorageCommand {
  action: 'plan_move' | 'apply_move' | 'analyze_location' | 'apply_location' | 'retry_storage' | 'cancel_storage' | 'reconcile_storage';
  record_id: string; storage_id: string; mapping_revision: string; version?: string;
  folder_name?: string; path?: string; strip_prefix?: string;
}
