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
