import type { Torrent } from './types';

export function storagePending(torrent: Torrent): boolean {
  return !!torrent.storage_state && torrent.storage_state !== 'ready';
}

export function storageLabel(torrent: Torrent): string {
  switch (torrent.storage_state) {
    case 'legacy': return 'Storage migration queued';
    case 'moving': return 'Storage migration in progress — copying and verifying files';
    case 'blocked': return 'Storage migration blocked';
    case 'ready': return '';
    default: return torrent.storage_state ? `Storage: ${torrent.storage_state}` : '';
  }
}

export function progressLabel(torrent: Torrent): string {
  switch (torrent.progress_source) {
    case 'last_known': return 'Last known — verification required';
    case 'legacy': return 'Legacy estimate — verification required';
    case 'unknown': return 'Progress unknown';
    case 'checking': return 'Verification in progress';
    default: return '';
  }
}

export function fileProgress(downloaded: number, size: number): number {
  if (size <= 0 || !Number.isFinite(size) || !Number.isFinite(downloaded)) return 0;
  return Math.max(0, Math.min(100, downloaded / size * 100));
}
