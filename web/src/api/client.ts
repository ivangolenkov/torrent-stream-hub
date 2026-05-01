import type { Torrent, File } from '../types';

const API_BASE = '/api/v1';

export const apiClient = {
  async getTorrents(): Promise<Torrent[]> {
    const response = await fetch(`${API_BASE}/torrents`);
    if (!response.ok) throw new Error(await readError(response, 'Failed to fetch torrents'));
    const data = await response.json();
    return Array.isArray(data) ? data : [];
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

  async uploadTorrent(file: globalThis.File): Promise<void> {
    const formData = new FormData();
    formData.append('file', file);

    const response = await fetch('/torrent/upload', {
      method: 'POST',
      body: formData
    });
    if (!response.ok) throw new Error(await readError(response, 'Failed to upload torrent file'));
  },

  async action(hash: string, action: 'pause' | 'resume' | 'delete' | 'recheck', deleteFiles: boolean = false): Promise<void> {
    const response = await fetch(`${API_BASE}/torrent/${hash}/action`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ action, delete_files: deleteFiles })
    });
    if (!response.ok) throw new Error(await readError(response, `Failed to perform action: ${action}`));
  },

  async getFiles(hash: string): Promise<File[]> {
    const response = await fetch(`${API_BASE}/torrent/${hash}/files`);
    if (!response.ok) throw new Error(await readError(response, 'Failed to fetch files'));
    const data = await response.json();
    return Array.isArray(data) ? data : [];
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

export function setupSSE(onUpdate: (torrents: Torrent[]) => void) {
  const eventSource = new EventSource(`${API_BASE}/events`);
  
  eventSource.onmessage = (event) => {
    try {
      const torrents = JSON.parse(event.data);
      onUpdate(torrents);
    } catch (e) {
      console.error('SSE JSON parse error', e);
    }
  };

  eventSource.onerror = (error) => {
    console.error('SSE Connection error', error);
  };

  return () => eventSource.close();
}
