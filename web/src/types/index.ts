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
  incoming_connectivity_note: string;
  torrents: BTTorrentHealth[];
}
