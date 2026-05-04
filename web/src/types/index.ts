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
  | '';

export interface File {
  index: number;
  path: string;
  size: number;
  offset: number;
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
  hash: string;
  name: string;
  title?: string;
  data?: string;
  poster?: string;
  category?: string;
  size: number;
  downloaded: number;
  progress: number;
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
  raw_download_speed: number;
  data_download_speed: number;
  useful_download_speed: number;
  waste_ratio: number;
  tracker_tiers_count: number;
  tracker_urls_count: number;
  metadata_ready: boolean;
  last_readd_source?: string;
  auto_hard_refresh_enabled: boolean;
  client_recycle_after_soft_fails: number;
  client_recycle_min_torrent_age_sec: number;
  recycle_scheduled_reason?: string;
  tracker_status?: string;
  tracker_error?: string;
  download_speed: number;
  upload_speed: number;
  degraded: boolean;
  last_refresh_at?: string;
  last_refresh_reason?: string;
  last_peer_refresh_at?: string;
  last_peer_refresh_reason?: string;
  last_healthy_at?: string;
  boosted_until?: string;
  max_established_conns: number;
  peak_connected: number;
  peak_seeds: number;
  peak_download_speed: number;
  peak_updated_at?: string;
  soft_refresh_count: number;
  hard_refresh_count: number;
  last_hard_refresh_at?: string;
  last_hard_refresh_reason?: string;
  last_hard_refresh_error?: string;
  hard_refresh_allowed: boolean;
  hard_refresh_blocked_reason?: string;
  active_streams: number;
  degradation_episode_started_at?: string;
  last_degraded_at?: string;
  last_recovered_at?: string;
  last_soft_refresh_at?: string;
  last_soft_refresh_reason?: string;
  soft_refresh_attempts_in_episode: number;
  hard_refresh_attempts_in_episode: number;
  last_soft_refresh_count_reset_reason?: string;
  next_hard_refresh_at?: string;
  next_client_recycle_at?: string;
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
  benchmark_mode: boolean;
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
  hard_refresh_enabled: boolean;
  auto_hard_refresh_enabled: boolean;
  hard_refresh_cooldown_sec: number;
  hard_refresh_after_soft_fails: number;
  client_recycle_enabled: boolean;
  client_recycle_cooldown_sec: number;
  client_recycle_after_hard_fails: number;
  client_recycle_after_soft_fails: number;
  client_recycle_min_torrent_age_sec: number;
  client_recycle_count: number;
  client_recycle_count_last_hour: number;
  last_client_recycle_at?: string;
  last_client_recycle_reason?: string;
  last_client_recycle_error?: string;
  client_recycle_allowed: boolean;
  client_recycle_blocked_reason?: string;
  next_client_recycle_at?: string;
  recycle_scheduled_reason?: string;
  last_restore_source?: string;
  last_restore_error?: string;
  invalid_metainfo_count: number;
  piece_completion_backend: string;
  piece_completion_persistent: boolean;
  piece_completion_error?: string;
  peer_drop_ratio: number;
  seed_drop_ratio: number;
  speed_drop_ratio: number;
  incoming_connectivity_note: string;
  torrents: BTTorrentHealth[];
}
