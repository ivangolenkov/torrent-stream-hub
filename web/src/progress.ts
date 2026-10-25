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
    case 'checking': return torrent.last_known_source && torrent.last_known_source !== 'unknown' && torrent.size > 0
      ? `Verification in progress; last known: ${fileProgress(torrent.last_known_bytes ?? 0, torrent.size).toFixed(1)}%`
      : 'Verification in progress';
    default: return '';
  }
}

export function selectionLabel(torrent: Torrent): string {
  const selection = torrent.selection;
  if (!selection) return torrent.selection_complete ? 'Selected files ready' : '';
  if (selection.files === 0) return 'No selected files';
  if (selection.size === 0) return selection.complete ? 'Selected empty files ready' : 'Selected empty files require inspection';
  return `Selected: ${selection.progress?.toFixed(1) ?? '0.0'}%${selection.complete ? ' — ready' : ''}; whole torrent: ${torrent.progress.toFixed(1)}%`;
}

export function activityLabel(torrent: Torrent): string {
  if (!torrent.metadata_known) return 'Fetching metadata';
  const op = torrent.storage?.operation;
  if (op && !['planned', 'analyzed', 'completed', 'cancelled'].includes(op.stage)) return op.kind === 'move' ? 'Moving files' : 'Checking existing data';
  if (torrent.verification && ['queued', 'running'].includes(torrent.verification.status)) {
    const v = torrent.verification;
    return `Verification ${v.status}: ${v.total_bytes ? `${fileProgress(v.checked_bytes, v.total_bytes).toFixed(1)}%` : 'empty payload'}; queued pieces: ${v.queued_pieces}`;
  }
  if (torrent.state === 'Queued' && !torrent.selection?.complete && !torrent.selection_complete && torrent.selection?.files !== 0) return 'Waiting for a download slot';
  if (torrent.state === 'Downloading' && torrent.download_speed === 0) return 'No transfer';
  return '';
}

export function fileProgress(downloaded: number, size: number): number {
  if (size <= 0 || !Number.isFinite(size) || !Number.isFinite(downloaded)) return 0;
  return Math.max(0, Math.min(100, downloaded / size * 100));
}
