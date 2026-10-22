import { beforeEach, expect, it, vi } from 'vitest';
import { createPinia, setActivePinia } from 'pinia';
import { apiClient, setupSSE } from '../src/api/client';
import { useTorrentStore } from '../src/stores/torrentStore';
import type { Torrent } from '../src/types';

vi.mock('../src/api/client', () => ({ apiClient: { getTorrents: vi.fn(), getFiles: vi.fn(), action: vi.fn(), setFilePriority: vi.fn(), setTorrentPriority: vi.fn() }, setupSSE: vi.fn(() => () => {}) }));
const fixture = (priority = 0) => ({ hash: 'one', state: 'Paused', files: [{ index: 0, priority }] }) as Torrent;
beforeEach(() => { vi.clearAllMocks(); setActivePinia(createPinia()); });

it('catches action/priority errors without changing confirmed state', async () => {
  const store = useTorrentStore(); store.torrents = [fixture()];
  vi.mocked(apiClient.setFilePriority).mockRejectedValueOnce(new Error('SQLite failed'));
  await store.setFilePriority('one', 0, -1);
  expect(store.actionErrors.one).toBe('SQLite failed');
  expect(store.torrents[0].files![0].priority).toBe(0);
  vi.mocked(apiClient.action).mockRejectedValueOnce(new Error('Conflict'));
  await store.performAction('one', 'resume');
  expect(store.actionErrors.one).toBe('Conflict');
  expect(store.actionPending.one).toBeUndefined();
});

it('does not apply an older REST response after SSE or stale priorities after commit', async () => {
  const store = useTorrentStore(); store.torrents = [fixture()]; store.initSSE();
  const receive = vi.mocked(setupSSE).mock.calls[0][0];
  let resolve!: (value: Torrent[]) => void;
  vi.mocked(apiClient.getTorrents).mockReturnValueOnce(new Promise(r => { resolve = r; }));
  const load = store.loadTorrents();
  receive([fixture(1)]); resolve([fixture(0)]); await load;
  expect(store.torrents[0].files![0].priority).toBe(1);
  vi.mocked(apiClient.setFilePriority).mockResolvedValueOnce(100);
  await store.setFilePriority('one', 0, -1);
  receive([{ ...fixture(-1), snapshot_at_ns: 50 }]); // Coincidentally matching old value must not clear the barrier.
  receive([{ ...fixture(0), snapshot_at_ns: 60 }]);
  expect(store.torrents[0].files![0].priority).toBe(-1);
  receive([{ ...fixture(-1), snapshot_at_ns: 110 }]);
  expect(store.torrents[0].files![0].priority).toBe(-1);
});
