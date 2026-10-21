import { defineStore } from 'pinia';
import { ref } from 'vue';
import { apiClient, setupSSE } from '../api/client';
import type { Torrent, File } from '../types';

export const useTorrentStore = defineStore('torrents', () => {
  const torrents = ref<Torrent[]>([]);
  const selectedHash = ref<string | null>(null);
  const files = ref<Record<string, File[]>>({});
  const pieces = ref<Record<string, string>>({});
  const pinning = ref<Record<string, boolean>>({});
  const pinErrors = ref<Record<string, string>>({});
  // Protect a confirmed pin from an older SSE event already in flight.
  const confirmedPermanent = new Set<string>();

  const applyTorrents = (data: Torrent[]) => {
    const hashes = new Set(data.map(t => t.hash));
    for (const hash of confirmedPermanent) {
      if (!hashes.has(hash)) confirmedPermanent.delete(hash);
    }
    for (const torrent of data) {
      if (!torrent.is_temporary) confirmedPermanent.delete(torrent.hash);
    }
    for (const hash of Object.keys(files.value)) {
      if (!hashes.has(hash)) delete files.value[hash];
    }
    for (const hash of Object.keys(pieces.value)) {
      if (!hashes.has(hash)) delete pieces.value[hash];
    }
    for (const hash of Object.keys(pinErrors.value)) {
      if (!hashes.has(hash)) delete pinErrors.value[hash];
    }
    if (selectedHash.value && !hashes.has(selectedHash.value)) selectedHash.value = null;
    torrents.value = data.map(t => confirmedPermanent.has(t.hash) ? { ...t, is_temporary: false } : t);
  };

  const makePermanent = async (hash: string) => {
    if (pinning.value[hash]) return;
    pinning.value[hash] = true;
    delete pinErrors.value[hash];
    try {
      await apiClient.action(hash, 'make_permanent');
      confirmedPermanent.add(hash);
      const torrent = torrents.value.find(t => t.hash === hash);
      if (torrent) torrent.is_temporary = false;
    } catch (e) {
      pinErrors.value[hash] = e instanceof Error ? e.message : 'Не удалось сохранить торрент';
    } finally {
      delete pinning.value[hash];
    }
  };
  
  let sseCleanup: (() => void) | null = null;

  const initSSE = () => {
    if (sseCleanup) return;
    sseCleanup = setupSSE((newData) => {
      applyTorrents(newData || []);
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
      applyTorrents(await apiClient.getTorrents());
    } catch (e) {
      console.error(e);
    }
  };

  const selectTorrent = async (hash: string) => {
    selectedHash.value = hash;
    if (!files.value[hash]) {
      try {
        const data = await apiClient.getFiles(hash);
        if (torrents.value.some(t => t.hash === hash)) files.value[hash] = data;
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
      confirmedPermanent.delete(hash);
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
    makePermanent,
    pinning,
    pinErrors,
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
