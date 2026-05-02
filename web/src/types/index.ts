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
  tracker_status?: string;
  tracker_error?: string;
  download_speed: number;
  upload_speed: number;
  degraded: boolean;
  last_refresh_at?: string;
  last_refresh_reason?: string;
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
  retrackers_mode: string;
  download_limit: number;
  upload_limit: number;
  swarm_watchdog_enabled: boolean;
  swarm_check_interval_sec: number;
  swarm_refresh_cooldown_sec: number;
  hard_refresh_enabled: boolean;
  hard_refresh_cooldown_sec: number;
  hard_refresh_after_soft_fails: number;
  client_recycle_enabled: boolean;
  client_recycle_cooldown_sec: number;
  client_recycle_after_hard_fails: number;
  client_recycle_count: number;
  client_recycle_count_last_hour: number;
  last_client_recycle_at?: string;
  last_client_recycle_reason?: string;
  last_client_recycle_error?: string;
  client_recycle_allowed: boolean;
  client_recycle_blocked_reason?: string;
  next_client_recycle_at?: string;
  peer_drop_ratio: number;
  seed_drop_ratio: number;
  speed_drop_ratio: number;
  incoming_connectivity_note: string;
  torrents: BTTorrentHealth[];
}
