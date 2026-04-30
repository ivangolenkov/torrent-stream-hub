import type { Torrent, File } from '../types';

const API_BASE = '/api/v1';

export const apiClient = {
  async getTorrents(): Promise<Torrent[]> {
    const response = await fetch(`${API_BASE}/torrents`);
    if (!response.ok) throw new Error('Failed to fetch torrents');
    return response.json();
  },

  async addTorrent(link: string, savePath: string = '', sequential: boolean = false): Promise<void> {
    const response = await fetch(`${API_BASE}/torrent/add`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ link, save_path: savePath, sequential })
    });
    if (!response.ok) throw new Error('Failed to add torrent');
  },

  async action(hash: string, action: 'pause' | 'resume' | 'delete' | 'recheck', deleteFiles: boolean = false): Promise<void> {
    const response = await fetch(`${API_BASE}/torrent/${hash}/action`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ action, delete_files: deleteFiles })
    });
    if (!response.ok) throw new Error(`Failed to perform action: ${action}`);
  },

  async getFiles(hash: string): Promise<File[]> {
    const response = await fetch(`${API_BASE}/torrent/${hash}/files`);
    if (!response.ok) throw new Error('Failed to fetch files');
    return response.json();
  }
};

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
