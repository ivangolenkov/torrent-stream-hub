import type { Torrent, File, BTHealth } from '../types';

const API_BASE = '/api/v1';

export type TorrentSnapshot = Torrent[] & { snapshotVersion?: string };
export type CommandReceipt = { commandAtNS: number; snapshotVersion?: string };
export type CommandResult = number | CommandReceipt;

async function commandReceipt(response: Response): Promise<CommandReceipt> {
  return { commandAtNS: (await response.json()).command_at_ns || 0, snapshotVersion: response.headers.get('X-Hub-Snapshot') || undefined };
}

export const apiClient = {
  async getTorrents(): Promise<TorrentSnapshot> {
    const response = await fetch(`${API_BASE}/torrents`);
    if (!response.ok) throw new Error(await readError(response, 'Failed to fetch torrents'));
    const data = await response.json();
    if (!Array.isArray(data)) throw new Error('Invalid torrent snapshot');
    Object.defineProperty(data, 'snapshotVersion', { value: response.headers.get('X-Hub-Snapshot') || undefined });
    return data;
  },

  async addTorrent(link: string, savePath: string = '', sequential: boolean = false, poster: string = ''): Promise<void> {
    const normalizedLink = normalizeTorrentLink(link);
    const response = await fetch(`${API_BASE}/torrent/add`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ link: normalizedLink, save_path: savePath, sequential, poster })
    });
    if (!response.ok) throw new Error(await readError(response, 'Failed to add torrent'));
  },

  async uploadTorrent(file: globalThis.File, poster: string = ''): Promise<void> {
    const formData = new FormData();
    formData.append('file', file);
    if (poster.trim()) formData.append('poster', poster.trim());

    const response = await fetch('/torrent/upload', {
      method: 'POST',
      body: formData
    });
    if (!response.ok) throw new Error(await readError(response, 'Failed to upload torrent file'));
  },

  async action(hash: string, action: 'pause' | 'resume' | 'delete' | 'recheck' | 'hard_refresh' | 'make_permanent', deleteFiles: boolean = false): Promise<CommandResult> {
    const response = await fetch(`${API_BASE}/torrent/${hash}/action`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ action, delete_files: deleteFiles })
    });
    if (!response.ok) throw new Error(await readError(response, `Failed to perform action: ${action}`));
    return commandReceipt(response);
  },

  async setTorrentPriority(hash: string, priority: number): Promise<CommandResult> {
    const response = await fetch(`${API_BASE}/torrent/${hash}/priority`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ priority })
    });
    if (!response.ok) throw new Error(await readError(response, 'Failed to set torrent priority'));
    return commandReceipt(response);
  },

  async setFilePriority(hash: string, index: number, priority: number): Promise<CommandResult> {
    const response = await fetch(`${API_BASE}/torrent/${hash}/file/${index}/priority`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ priority })
    });
    if (!response.ok) throw new Error(await readError(response, 'Failed to set file priority'));
    return commandReceipt(response);
  },

  async getFiles(hash: string): Promise<File[]> {
    const response = await fetch(`${API_BASE}/torrent/${hash}/files`);
    if (!response.ok) throw new Error(await readError(response, 'Failed to fetch files'));
    const data = await response.json();
    return Array.isArray(data) ? data : [];
  },

  async getPieces(hash: string): Promise<string> {
    const response = await fetch(`${API_BASE}/torrent/${hash}/pieces`);
    if (!response.ok) throw new Error(await readError(response, 'Failed to fetch pieces'));
    return await response.text();
  },

  async getBTHealth(): Promise<BTHealth> {
    const response = await fetch(`${API_BASE}/health/bt`);
    if (!response.ok) throw new Error(await readError(response, 'Failed to fetch BitTorrent health'));
    return await response.json();
  },

  async recycleBTClient(): Promise<void> {
    const response = await fetch(`${API_BASE}/health/bt/recycle`, { method: 'POST' });
    if (!response.ok) throw new Error(await readError(response, 'Failed to recycle BitTorrent client'));
  },

  torrentDownloadUrl(hash: string): string {
    return `${API_BASE}/torrent/${encodeURIComponent(hash)}/download`;
  },

  fileDownloadUrl(hash: string, index: number): string {
    return `${API_BASE}/torrent/${encodeURIComponent(hash)}/file/${index}/download`;
  }
};

async function readError(response: Response, fallback: string) {
  const text = await response.text().catch(() => '');
  if (!text) return fallback;

  try {
    const data = JSON.parse(text);
    if (data?.error) return data.error;
  } catch {
    return text;
  }

  return fallback;
}

function normalizeTorrentLink(link: string) {
  const trimmed = link.trim();
  if (trimmed.startsWith('magnet:')) return trimmed;
  if (/^[a-fA-F0-9]{40}$/.test(trimmed) || /^[a-zA-Z2-7]{32}$/.test(trimmed)) {
    return `magnet:?xt=urn:btih:${trimmed}`;
  }
  return trimmed;
}

export function setupSSE(onUpdate: (torrents: Torrent[], version?: string) => void) {
  const eventSource = new EventSource(`${API_BASE}/events`);
  let stopped = false;
  
  eventSource.onmessage = (event) => {
    if (stopped) return;
    try {
      const torrents = JSON.parse(event.data);
      if (!Array.isArray(torrents)) throw new Error('Invalid SSE torrent snapshot');
      onUpdate(torrents, event.lastEventId || undefined);
    } catch (e) {
      console.error('SSE JSON parse error', e);
    }
  };

  eventSource.onerror = (error) => {
    console.error('SSE Connection error', error);
  };

  return () => { stopped = true; eventSource.close(); };
}
