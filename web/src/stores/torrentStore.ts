import { defineStore } from 'pinia';
import { ref } from 'vue';
import { apiClient, setupSSE } from '../api/client';
import type { Torrent, File } from '../types';

export const useTorrentStore = defineStore('torrents', () => {
  const torrents = ref<Torrent[]>([]);
  const selectedHash = ref<string | null>(null);
  const files = ref<Record<string, File[]>>({});
  const pieces = ref<Record<string, string>>({});
  
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

  const fetchPieces = async (hash: string) => {
    try {
      pieces.value[hash] = await apiClient.getPieces(hash);
    } catch (e) {
      console.error(e);
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
    if (action === 'delete') {
      delete files.value[hash];
    }
    await loadTorrents();
  };

  const setTorrentPriority = async (hash: string, priority: number) => {
    await apiClient.setTorrentPriority(hash, priority);
    // Reload files for this torrent if it's currently selected to update UI
    if (files.value[hash]) {
      try {
        files.value[hash] = await apiClient.getFiles(hash);
      } catch (e) {
        console.error(e);
      }
    }
    await loadTorrents();
  };

  const setFilePriority = async (hash: string, index: number, priority: number) => {
    await apiClient.setFilePriority(hash, index, priority);
    // Update local file state
    if (files.value[hash]) {
      const file = files.value[hash].find(f => f.index === index);
      if (file) {
        file.priority = priority;
      }
    }
  };

  return {
    torrents,
    selectedHash,
    files,
    pieces,
    initSSE,
    stopSSE,
    loadTorrents,
    selectTorrent,
    fetchPieces,
    closeInspector,
    performAction,
    setTorrentPriority,
    setFilePriority,
  };
});
