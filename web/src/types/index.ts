import type { StorageRecovery } from './storageRecovery';
import type { StorageLocation, FileStatus, SelectionProgress, VerificationProgress } from './storage';

export type TorrentState = 
  | 'Queued'
  | 'Downloading'
  | 'Streaming'
  | 'Seeding'
  | 'Paused'
  | 'Error'
  | 'MissingFiles'
  | 'DiskFull';

export type ErrorReason = 
  | 'Invalid torrent'
  | 'Tracker unreachable'
  | 'No peers'
  | 'Disk full'
  | 'Missing files'
	| 'Verification failed'
	| 'Activation blocked'
	| 'metadata persistence failed'
	| 'Invalid file selection'
  | '';

export interface File {
  metadata_path?: string;
  status?: FileStatus;
  index: number;
  path: string;
  size: number;
  offset: number;
	 offset_known?: boolean;
	 legacy_downloaded?: number;
  downloaded: number;
  priority: number;
  is_media: boolean;
}

export interface PeerSummary {
  known: number;
  connected: number;
  pending: number;
  half_open: number;
  seeds: number;
  metadata_ready: boolean;
  tracker_status?: string;
  tracker_error?: string;
  dht_status?: string;
}

export interface Torrent {
	resume?: ResumeProgress;
 direct_read_ready?: boolean;
 selection?: SelectionProgress;
 verification?: VerificationProgress;
 last_known_bytes?: number;
 last_known_at_ms?: number;
 last_known_source?: string;
 storage?: StorageLocation;
 catalog_version?: string;
 snapshot_freshness?: string;
	control?: {
		record_id: string;
		desired_run: 'paused' | 'enabled';
		desired_revision: string;
		applied_revision: string;
		runtime_attachment?: string;
		reconcile_pending: boolean;
		reconcile_reason?: string;
		apply_failure?: {
			record_id: string;
			revision: string;
			code: 'intent_commit_failed' | 'runtime_stage_failed' | 'runtime_apply_failed' | 'runtime_bind_failed';
			retry: 'explicit_pause_or_resume';
		};
		activation_blocks: { kind: 'verification' | 'missing_files' | 'legacy_error' | 'metadata_persistence' | 'invalid_selection'; reason: string }[];
	};
	download_block?: string;
	deletion_mode?: string;
	deletion_error?: string;
	storage_state?: string;
	storage_error?: string;
	storage_recovery?: StorageRecovery;
  is_temporary: boolean;
  hash: string;
  name: string;
  title?: string;
  data?: string;
  poster?: string;
  category?: string;
  size: number;
  downloaded: number;
  progress: number;
	progress_source?: 'verified' | 'last_known' | 'legacy' | 'unknown' | 'checking';
	progress_observed_at_ns?: number;
	legacy_downloaded?: number;
	metadata_known?: boolean;
	selection_complete?: boolean;
	snapshot_at_ns?: number;
	recheck?: { id: number; status: 'pending' | 'running' | 'succeeded' | 'failed' | 'cancelled'; error?: string };
	activity?: { streaming: boolean; streams: number; direct_readers: number; preloads: number; debouncing: boolean; verifying: boolean };
  state: TorrentState;
  error?: ErrorReason;
  files?: File[];
  piece_length: number;
  num_pieces: number;
  download_speed: number;
  upload_speed: number;
  peers: number;
  seeds: number;
  peer_summary: PeerSummary;
}

export interface ResumeProgress {
  mode: 'fast' | 'verify';
  status: 'pending' | 'restored' | 'partial' | 'fallback' | 'disabled';
  reason: string;
  checkpoint_generation?: string;
  checkpoint_at_ms?: number;
  restored_bytes: number;
  freshly_verified_bytes: number;
}

export interface BTTorrentHealth {
  hash: string;
  name: string;
  state: TorrentState;
  known: number;
  connected: number;
  pending: number;
  half_open: number;
  seeds: number;
  bytes_read: number;
  bytes_read_data: number;
  bytes_read_useful_data: number;
  bytes_written: number;
  bytes_written_data: number;
  chunks_read: number;
  chunks_read_useful: number;
  chunks_read_wasted: number;
  pieces_dirtied_good: number;
  pieces_dirtied_bad: number;
  waste_ratio: number;
  tracker_tiers_count: number;
  tracker_urls_count: number;
  metadata_ready: boolean;
  tracker_status?: string;
  tracker_error?: string;
  download_speed: number;
  upload_speed: number;
  degraded: boolean;
  peak_download_speed: number;
  peak_updated_at?: string;
  last_refresh_at?: string;
  last_refresh_reason?: string;
  last_peer_refresh_at?: string;
  last_peer_refresh_reason?: string;
  last_healthy_at?: string;
  boosted_until?: string;
  max_established_conns: number;
  active_streams: number;
}

export interface BTHealth {
  seed_enabled: boolean;
  upload_enabled: boolean;
  dht_enabled: boolean;
  pex_enabled: boolean;
  upnp_enabled: boolean;
  tcp_enabled: boolean;
  utp_enabled: boolean;
  ipv6_enabled: boolean;
  listen_port: number;
  client_profile: string;
  download_profile: string;
  established_conns_per_torrent: number;
  half_open_conns_per_torrent: number;
  total_half_open_conns: number;
  peers_low_water: number;
  peers_high_water: number;
  dial_rate_limit: number;
  public_ip_discovery_enabled: boolean;
  public_ipv4_status: string;
  public_ipv6_status: string;
  retrackers_mode: string;
  download_limit: number;
  upload_limit: number;
  swarm_watchdog_enabled: boolean;
  swarm_check_interval_sec: number;
  swarm_refresh_cooldown_sec: number;
  invalid_metainfo_count: number;
  piece_completion_backend: string;
  piece_completion_persistent: boolean;
  piece_completion_error?: string;
  piece_completion_dropped: number;
  piece_completion_pending: number;
  peer_drop_ratio: number;
  seed_drop_ratio: number;
  speed_drop_ratio: number;
  incoming_connectivity_note: string;
  torrents: BTTorrentHealth[];
}
