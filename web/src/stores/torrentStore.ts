import { defineStore } from 'pinia';
import { ref } from 'vue';
import { apiClient, setupSSE } from '../api/client';
import type { CommandResult } from '../api/client';
import { SnapshotOrder } from './snapshotOrder';
import type { Torrent, File } from '../types';
import type { StorageRecoveryAction } from '../types/storageRecovery';

export const useTorrentStore = defineStore('torrents', () => {
  const torrents = ref<Torrent[]>([]);
  const selectedHash = ref<string | null>(null);
  const files = ref<Record<string, File[]>>({});
  const pieces = ref<Record<string, string>>({});
  const pinning = ref<Record<string, boolean>>({});
  const pinErrors = ref<Record<string, string>>({});
  const actionErrors = ref<Record<string, string>>({});
  const actionPending = ref<Record<string, boolean>>({});
  const recoveryPending = ref<Record<string, boolean>>({});
  let dataVersion = 0;
  let listRequest = 0;
  const detailVersions = new Map<string, string>();
  const detailRequests = new Map<string, string>();
  const detailsIdentity = (t?: Torrent) => t ? `${t.control?.record_id || ''}/${t.catalog_version || ''}/${t.control?.desired_revision || ''}` : '';
  const priorityGeneration = new Map<string, number>();
  const detailsKey = (t?: Torrent) => t ? `${priorityGeneration.get(t.hash) || 0}/${snapshotOrder.epoch || ''}/${detailsIdentity(t)}/${t.metadata_known}/${t.downloaded}/${t.progress_source}/${t.recheck?.status || ''}` : '';

  const confirmedPriorities = new Map<string, { values: Map<number, number>; recordID?: string; commandAt: number; snapshotVersion?: string }>();
  const commandBarriers = new Map<string, number>();
  // Protect a confirmed pin from an older SSE event already in flight.
  const confirmedPermanent = new Set<string>();
  const snapshotOrder = new SnapshotOrder();
  const recordID = (hash: string) => torrents.value.find(t => t.hash === hash)?.control?.record_id;
  const sameRecord = (hash: string, issuedID?: string) => !issuedID || issuedID === recordID(hash);
  const clearEpochCaches = () => {
    confirmedPriorities.clear(); priorityGeneration.clear(); confirmedPermanent.clear(); commandBarriers.clear(); detailVersions.clear(); detailRequests.clear();
    files.value = {}; pieces.value = {}; pinErrors.value = {}; actionErrors.value = {};
  };
  const receipt = (result: CommandResult, issuedEpoch?: string) => {
    const version = typeof result === 'number' ? undefined : result.snapshotVersion;
    const oldEpoch = snapshotOrder.epoch;
    if (!snapshotOrder.acceptReceipt(version, issuedEpoch)) return undefined;
    if (oldEpoch && snapshotOrder.epoch !== oldEpoch) clearEpochCaches();
    dataVersion++;
    return typeof result === 'number' ? result : result.commandAtNS;
  };

  const applyTorrents = (data: Torrent[], version?: string, fromREST = false) => {
    const order = snapshotOrder.acceptSnapshot(version, fromREST);
    if (order === 'refresh') { void loadTorrents(); return; }
    if (order === 'stale') return;
    if (order === 'changed') clearEpochCaches();
    dataVersion++;
    data = data.flatMap(t => {
      const barrier = commandBarriers.get(t.hash) || 0;
      const previous = torrents.value.find(old => old.hash === t.hash);
      if (!version && t.snapshot_at_ns && t.snapshot_at_ns < barrier) return previous ? [previous] : [];
      if (previous?.control && t.control) {
        if (previous.control.record_id === t.control.record_id) {
          // Revisions are decimal strings, not lossy JS numbers. Only compare
          // within one record identity; re-add starts an unrelated revision.
          const old = previous.control.desired_revision;
          const next = t.control.desired_revision;
          if (next.length < old.length || (next.length === old.length && next < old)) return [previous];
        } else {
          confirmedPriorities.delete(t.hash);
          confirmedPermanent.delete(t.hash);
          delete files.value[t.hash];
          delete pieces.value[t.hash];
        }
      }
      const confirmed = confirmedPriorities.get(t.hash);
      // acceptSnapshot has already fenced this version after every accepted
      // receipt. The next authoritative snapshot may contain another client's
      // command; it need not repeat our priority before superseding it.
      if (confirmed && (version || (t.snapshot_at_ns && t.snapshot_at_ns >= confirmed.commandAt) ||
          (confirmed.recordID && t.control?.record_id !== confirmed.recordID))) {
        confirmedPriorities.delete(t.hash);
        detailVersions.delete(t.hash);
      }
      const priorities = confirmedPriorities.get(t.hash)?.values;
      return [{ ...t, files: t.files?.map(f => priorities?.has(f.index) ? { ...f, priority: priorities.get(f.index)! } : f) }];
    });
    const hashes = new Set(data.map(t => t.hash));
    for (const cache of [priorityGeneration, detailVersions, detailRequests]) {
      for (const hash of cache.keys()) if (!hashes.has(hash)) cache.delete(hash);
    }
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
    for (const t of torrents.value) if (t.files && selectedHash.value===t.hash) { files.value[t.hash] = t.files; detailVersions.set(t.hash, detailsKey(t)); }
    if (selectedHash.value) void loadDetails(selectedHash.value);
  };

  const makePermanent = async (hash: string) => {
    if (pinning.value[hash]) return;
    pinning.value[hash] = true;
    delete pinErrors.value[hash];
    const issuedEpoch = snapshotOrder.epoch;
    const issuedID = recordID(hash);
    try {
      const result = await apiClient.action(hash, 'make_permanent');
      if (!sameRecord(hash, issuedID) || receipt(result, issuedEpoch) === undefined) { await loadTorrents(); return; }
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
    sseCleanup = setupSSE((newData, version) => {
      applyTorrents(newData || [], version);
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
      if (request === listRequest && version === dataVersion) applyTorrents(data, data.snapshotVersion, true);
    } catch (e) {
      console.error(e);
    }
  };

  const loadDetails = async (hash: string) => {
    const torrent = torrents.value.find(t => t.hash === hash);
    if (!torrent || selectedHash.value !== hash) return;
    const key = detailsKey(torrent);
    if (detailVersions.get(hash) === key || detailRequests.get(hash) === key) return;
    const identity = detailsIdentity(torrent);
    detailRequests.set(hash, key);
    try {
      const data = await apiClient.getFiles(hash);
      if (selectedHash.value !== hash || detailsKey(torrents.value.find(t => t.hash === hash)) !== key) return;
      if (data.detailsVersion && data.detailsVersion !== identity) return;
      const priorities = confirmedPriorities.get(hash)?.values;
      files.value[hash] = data.map(f => priorities?.has(f.index) ? { ...f, priority: priorities.get(f.index)! } : f);
      detailVersions.set(hash, key);
    } catch (e) { console.error(e); }
    finally { if (detailRequests.get(hash) === key) detailRequests.delete(hash); }
  };

  const selectTorrent = async (hash: string) => {
    selectedHash.value = hash;
    await loadDetails(hash);
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
    const issuedEpoch = snapshotOrder.epoch;
    const issuedID = recordID(hash);
    const result = await apiClient.action(hash, action, deleteFiles);
    if (!sameRecord(hash, issuedID)) { await loadTorrents(); return; }
    const barrier = receipt(result, issuedEpoch);
    if (barrier === undefined) { await loadTorrents(); return; }
    if (barrier && !snapshotOrder.epoch) commandBarriers.set(hash, barrier);
    if (action === 'delete' && selectedHash.value === hash) {
      closeInspector();
    }
    if (action === 'delete') {
      torrents.value = torrents.value.filter(t => t.hash !== hash);
      confirmedPermanent.delete(hash);
      confirmedPriorities.delete(hash);
      priorityGeneration.delete(hash);
      delete files.value[hash];
    }
    await loadTorrents();
  });

  const recoverStorage = async (hash: string, action: StorageRecoveryAction, choice?: 'final' | 'part') => {
    if (recoveryPending.value[hash]) return false;
    recoveryPending.value[hash] = true;
    const issuedID = recordID(hash);
    delete actionErrors.value[hash];
    try {
    const recovery = torrents.value.find(t => t.hash === hash)?.storage_recovery;
    if (!recovery || !recovery.actions.includes(action)) throw new Error('Refresh the current recovery operation before retrying');
    const issuedEpoch = snapshotOrder.epoch;
    const result = await apiClient.recoveryAction(hash, recovery, action, choice);
    if (!sameRecord(hash, recovery.record_id)) { await loadTorrents(); return false; }
    receipt(result, issuedEpoch);
    await loadTorrents();
    return true;
    } catch (e) {
      if (sameRecord(hash, issuedID)) actionErrors.value[hash] = e instanceof Error ? e.message : 'Storage recovery failed';
      await loadTorrents();
      return false;
    } finally { delete recoveryPending.value[hash]; }
  };

  const setTorrentPriority = async (hash: string, priority: number) => runCommand(hash, async () => {
    const issuedEpoch = snapshotOrder.epoch;
    const issuedID = recordID(hash);
    const result = await apiClient.setTorrentPriority(hash, priority);
    if (!sameRecord(hash, issuedID)) { await loadTorrents(); return; }
    const barrier = receipt(result, issuedEpoch);
    if (barrier === undefined) { await loadTorrents(); return; }
    if (barrier && !snapshotOrder.epoch) commandBarriers.set(hash, barrier);
    const current = torrents.value.find(t => t.hash === hash)?.files || files.value[hash] || [];
    confirmedPriorities.set(hash, { values: new Map(current.map(f => [f.index, priority])), recordID: issuedID, commandAt: barrier, snapshotVersion: typeof result === 'number' ? undefined : result.snapshotVersion });
    priorityGeneration.set(hash, (priorityGeneration.get(hash) || 0) + 1);
    for (const file of current) file.priority = priority;
    await loadTorrents();
  });

  const setFilePriority = async (hash: string, index: number, priority: number) => runCommand(hash, async () => {
    const issuedEpoch = snapshotOrder.epoch;
    const issuedID = recordID(hash);
    const result = await apiClient.setFilePriority(hash, index, priority);
    if (!sameRecord(hash, issuedID)) { await loadTorrents(); return; }
    const barrier = receipt(result, issuedEpoch);
    if (barrier === undefined) { await loadTorrents(); return; }
    if (barrier && !snapshotOrder.epoch) commandBarriers.set(hash, barrier);
    const priorities = confirmedPriorities.get(hash)?.values || new Map<number, number>();
    priorities.set(index, priority);
    confirmedPriorities.set(hash, { values: priorities, recordID: issuedID, commandAt: barrier, snapshotVersion: typeof result === 'number' ? undefined : result.snapshotVersion });
    priorityGeneration.set(hash, (priorityGeneration.get(hash) || 0) + 1);
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
    recoverStorage,
    recoveryPending,
    setTorrentPriority,
    setFilePriority,
  };
});
