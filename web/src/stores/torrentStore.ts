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
  const actionErrors = ref<Record<string, string>>({});
  const actionPending = ref<Record<string, boolean>>({});
  let dataVersion = 0;
  let listRequest = 0;
  const confirmedPriorities = new Map<string, Map<number, number>>();
  const commandBarriers = new Map<string, number>();
  // Protect a confirmed pin from an older SSE event already in flight.
  const confirmedPermanent = new Set<string>();

  const applyTorrents = (data: Torrent[]) => {
    dataVersion++;
    data = data.flatMap(t => {
      const barrier = commandBarriers.get(t.hash) || 0;
      const previous = torrents.value.find(old => old.hash === t.hash);
      if (t.snapshot_at_ns && t.snapshot_at_ns < barrier) return previous ? [previous] : [];
      const priorities = confirmedPriorities.get(t.hash);
      if (priorities && t.files && [...priorities].every(([index, priority]) => t.files?.some(f => f.index === index && f.priority === priority))) confirmedPriorities.delete(t.hash);
      return [{ ...t, files: t.files?.map(f => priorities?.has(f.index) ? { ...f, priority: priorities.get(f.index)! } : f) }];
    });
    const hashes = new Set(data.map(t => t.hash));
    for (const hash of confirmedPriorities.keys()) if (!hashes.has(hash)) confirmedPriorities.delete(hash);
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
    const request = ++listRequest;
    const version = dataVersion;
    try {
      const data = await apiClient.getTorrents();
      if (request === listRequest && version === dataVersion) applyTorrents(data);
    } catch (e) {
      console.error(e);
    }
  };

  const selectTorrent = async (hash: string) => {
    selectedHash.value = hash;
    const version = dataVersion;
    if (!files.value[hash]) {
      try {
        const data = await apiClient.getFiles(hash);
        if (version === dataVersion && selectedHash.value === hash && torrents.value.some(t => t.hash === hash)) files.value[hash] = data;
      } catch (e) {
        console.error(e);
      }
    }
  };

  const fetchPieces = async (hash: string) => {
    const version = dataVersion;
    try {
      const data = await apiClient.getPieces(hash);
      if (version === dataVersion && selectedHash.value === hash) pieces.value[hash] = data;
    } catch (e) {
      console.error(e);
    }
  };

  const closeInspector = () => {
    selectedHash.value = null;
  };

  // Actions wrapper
  const runCommand = async (hash: string, command: () => Promise<void>) => {
    if (actionPending.value[hash]) return false;
    actionPending.value[hash] = true;
    delete actionErrors.value[hash];
    dataVersion++;
    try { await command(); return true; }
    catch (e) { actionErrors.value[hash] = e instanceof Error ? e.message : 'Operation failed'; return false; }
    finally { delete actionPending.value[hash]; }
  };

  const performAction = async (hash: string, action: 'pause' | 'resume' | 'delete' | 'recheck', deleteFiles = false) => runCommand(hash, async () => {
    const barrier = await apiClient.action(hash, action, deleteFiles);
    if (barrier) commandBarriers.set(hash, barrier);
    if (action === 'delete' && selectedHash.value === hash) {
      closeInspector();
    }
    if (action === 'delete') {
      confirmedPermanent.delete(hash);
      delete files.value[hash];
    }
    await loadTorrents();
  });

  const setTorrentPriority = async (hash: string, priority: number) => runCommand(hash, async () => {
    const barrier = await apiClient.setTorrentPriority(hash, priority);
    if (barrier) commandBarriers.set(hash, barrier);
    const current = torrents.value.find(t => t.hash === hash)?.files || files.value[hash] || [];
    confirmedPriorities.set(hash, new Map(current.map(f => [f.index, priority])));
    for (const file of current) file.priority = priority;
    await loadTorrents();
  });

  const setFilePriority = async (hash: string, index: number, priority: number) => runCommand(hash, async () => {
    const barrier = await apiClient.setFilePriority(hash, index, priority);
    if (barrier) commandBarriers.set(hash, barrier);
    const priorities = confirmedPriorities.get(hash) || new Map<number, number>();
    priorities.set(index, priority);
    confirmedPriorities.set(hash, priorities);
    // Update local file state
    if (files.value[hash]) {
      const file = files.value[hash].find(f => f.index === index);
      if (file) {
        file.priority = priority;
      }
    }
    const file = torrents.value.find(t => t.hash === hash)?.files?.find(f => f.index === index);
    if (file) file.priority = priority;
  });

  return {
    makePermanent,
    pinning,
    pinErrors,
    actionErrors,
    actionPending,
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
