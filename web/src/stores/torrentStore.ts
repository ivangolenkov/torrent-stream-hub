import { defineStore } from 'pinia';
import { ref } from 'vue';
import { apiClient, setupSSE } from '../api/client';
import type { Torrent, File } from '../types';

export const useTorrentStore = defineStore('torrents', () => {
  const torrents = ref<Torrent[]>([]);
  const selectedHash = ref<string | null>(null);
  const files = ref<Record<string, File[]>>({});
  
  let sseCleanup: (() => void) | null = null;

  const initSSE = () => {
    if (sseCleanup) return;
    sseCleanup = setupSSE((newData) => {
      torrents.value = newData || [];
    });
  };

  const stopSSE = () => {
    if (sseCleanup) {
      sseCleanup();
      sseCleanup = null;
    }
  };

  const loadTorrents = async () => {
    try {
      torrents.value = await apiClient.getTorrents();
    } catch (e) {
      console.error(e);
    }
  };

  const selectTorrent = async (hash: string) => {
    selectedHash.value = hash;
    if (!files.value[hash]) {
      try {
        files.value[hash] = await apiClient.getFiles(hash);
      } catch (e) {
        console.error(e);
      }
    }
  };

  const closeInspector = () => {
    selectedHash.value = null;
  };

  // Actions wrapper
  const performAction = async (hash: string, action: 'pause' | 'resume' | 'delete', deleteFiles = false) => {
    await apiClient.action(hash, action, deleteFiles);
    if (action === 'delete' && selectedHash.value === hash) {
      closeInspector();
    }
    await loadTorrents();
  };

  return {
    torrents,
    selectedHash,
    files,
    initSSE,
    stopSSE,
    loadTorrents,
    selectTorrent,
    closeInspector,
    performAction,
  };
});
