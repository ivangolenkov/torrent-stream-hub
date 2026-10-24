import { beforeEach, expect, it, vi } from 'vitest';
import { createPinia, setActivePinia } from 'pinia';
import { apiClient, setupSSE } from '../src/api/client';
import { useTorrentStore } from '../src/stores/torrentStore';
import type { Torrent } from '../src/types';
import { flushPromises } from '@vue/test-utils';

vi.mock('../src/api/client', () => ({ apiClient: { getTorrents: vi.fn(), getFiles: vi.fn(), action: vi.fn(), setFilePriority: vi.fn(), setTorrentPriority: vi.fn() }, setupSSE: vi.fn(() => () => {}) }));
const fixture = (priority = 0) => ({ hash: 'one', state: 'Paused', files: [{ index: 0, priority }] }) as Torrent;
beforeEach(() => { vi.clearAllMocks(); setActivePinia(createPinia()); });

it('orders full precision desired revisions only within the same record identity', () => {
  const store = useTorrentStore(); store.initSSE();
  const receive = vi.mocked(setupSSE).mock.calls[0][0];
  const versioned = (record: string, revision: string, priority: number): Torrent => ({ ...fixture(priority), control: { record_id: record, desired_run: 'paused', desired_revision: revision, applied_revision: '0', reconcile_pending: true, activation_blocks: [] } });
  receive([versioned('first', '9007199254740993', -1)]);
  receive([versioned('first', '9007199254740992', 0)]);
  expect(store.torrents[0].files![0].priority).toBe(-1);
  receive([versioned('first', '10000000000000000', 1)]);
  expect(store.torrents[0].files![0].priority).toBe(1);
  receive([versioned('re-added', '1', 0)]);
  expect(store.torrents[0].control?.record_id).toBe('re-added');
  expect(store.torrents[0].files![0].priority).toBe(0);
});

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

const epochA = 'aaaaaaaaaaaaaaaa', epochB = 'bbbbbbbbbbbbbbbb';
const snapshot = (items: Torrent[], version: string) => Object.assign(items, { snapshotVersion: version });

it('loads compact details only for the selected torrent and refreshes on revision changes', async () => {
  const store = useTorrentStore(); store.initSSE();
  const receive = vi.mocked(setupSSE).mock.calls[0][0];
  const compact = (revision: string): Torrent => ({ ...fixture(), files: undefined, catalog_version: 'catalog', control: { record_id: 'record', desired_run: 'paused', desired_revision: revision, applied_revision: revision, reconcile_pending: false, activation_blocks: [] } });
  receive([compact('1')], `${epochA}/1`);
  expect(apiClient.getFiles).not.toHaveBeenCalled();
  vi.mocked(apiClient.getFiles).mockResolvedValueOnce(fixture().files!);
  await store.selectTorrent('one');
  receive([compact('1')], `${epochA}/2`); await flushPromises();
  expect(apiClient.getFiles).toHaveBeenCalledTimes(1);
  vi.mocked(apiClient.getFiles).mockResolvedValueOnce(fixture(1).files!);
  receive([compact('2')], `${epochA}/3`); await flushPromises();
  expect(apiClient.getFiles).toHaveBeenCalledTimes(2);
  expect(store.files.one[0].priority).toBe(1);
});

it('does not resurrect a deleted hash from an old list or erase re-add with an old empty list', async () => {
  const store = useTorrentStore(); store.initSSE();
  const receive = vi.mocked(setupSSE).mock.calls[0][0];
  receive([fixture()], `${epochA}/1`);
  vi.mocked(apiClient.action).mockResolvedValueOnce({ commandAtNS: 1, snapshotVersion: `${epochA}/3` });
  vi.mocked(apiClient.getTorrents).mockResolvedValueOnce(snapshot([], `${epochA}/4`));
  await store.performAction('one', 'delete');
  receive([fixture()], `${epochA}/2`);
  expect(store.torrents).toHaveLength(0);
  receive([{ ...fixture(1), activity: { streaming: true, streams: 1, direct_readers: 0, preloads: 0, debouncing: false, verifying: false } }], `${epochA}/5`);
  receive([], `${epochA}/4`);
  receive([fixture(-1)], `${epochA}/2`);
  expect(store.torrents[0].files![0].priority).toBe(1);
  expect(store.torrents[0].activity?.streaming).toBe(true);
});

it('confirms a new process epoch via REST and ignores delayed old process events', async () => {
  const store = useTorrentStore(); store.initSSE();
  const receive = vi.mocked(setupSSE).mock.calls[0][0];
  receive([fixture()], `${epochA}/99`);
  store.files.one = fixture().files!;
  vi.mocked(apiClient.getTorrents).mockResolvedValueOnce(snapshot([fixture(1)], `${epochB}/2`));
  receive([], `${epochB}/1`);
  await flushPromises();
  expect(store.torrents[0].files![0].priority).toBe(1);
  expect(store.files.one).toBeUndefined();
  vi.mocked(apiClient.getTorrents).mockResolvedValueOnce(snapshot([fixture(1)], `${epochB}/3`));
  receive([fixture(-1)], `${epochA}/100`);
  await flushPromises();
  expect(store.torrents[0].files![0].priority).toBe(1);
});

it('does not apply a delayed old-epoch priority acknowledgment to a fresh record', async () => {
  const store = useTorrentStore(); store.initSSE();
  const receive = vi.mocked(setupSSE).mock.calls[0][0];
  receive([fixture()], `${epochA}/1`);
  let finish!: (value: { commandAtNS: number; snapshotVersion: string }) => void;
  vi.mocked(apiClient.setFilePriority).mockReturnValueOnce(new Promise(resolve => { finish = resolve; }));
  const command = store.setFilePriority('one', 0, -1);
  vi.mocked(apiClient.getTorrents).mockResolvedValueOnce(snapshot([fixture(1)], `${epochB}/2`));
  receive([], `${epochB}/1`); await flushPromises();
  vi.mocked(apiClient.getTorrents).mockResolvedValueOnce(snapshot([fixture(1)], `${epochB}/3`));
  finish({ commandAtNS: 2, snapshotVersion: `${epochA}/3` }); await command;
  expect(store.torrents[0].files![0].priority).toBe(1);
});

it('ignores a late command acknowledgment after delete/re-add within the same epoch', async () => {
  const store = useTorrentStore(); store.initSSE();
  const receive = vi.mocked(setupSSE).mock.calls[0][0];
  const record = (id: string, priority: number): Torrent => ({ ...fixture(priority), control: { record_id: id, desired_run: 'paused', desired_revision: '1', applied_revision: '1', reconcile_pending: false, activation_blocks: [] } });
  receive([record('old', 0)], `${epochA}/1`);
  let finish!: (value: { commandAtNS: number; snapshotVersion: string }) => void;
  vi.mocked(apiClient.setFilePriority).mockReturnValueOnce(new Promise(resolve => { finish = resolve; }));
  const command = store.setFilePriority('one', 0, -1);
  receive([record('new', 1)], `${epochA}/3`);
  vi.mocked(apiClient.getTorrents).mockResolvedValueOnce(snapshot([record('new', 1)], `${epochA}/5`));
  finish({ commandAtNS: 2, snapshotVersion: `${epochA}/4` }); await command;
  expect(store.torrents[0].control?.record_id).toBe('new');
  expect(store.torrents[0].files![0].priority).toBe(1);
});

it('accepts another client priority after our receipt even when the intermediate snapshot was missed', async () => {
  const store = useTorrentStore(); store.initSSE();
  const receive = vi.mocked(setupSSE).mock.calls[0][0];
  receive([fixture()], `${epochA}/1`);
  vi.mocked(apiClient.setFilePriority).mockResolvedValueOnce({ commandAtNS: 100, snapshotVersion: `${epochA}/3` });
  await store.setFilePriority('one', 0, -1);
  receive([fixture(0)], `${epochA}/2`);
  expect(store.torrents[0].files![0].priority).toBe(-1);
  receive([fixture(1)], `${epochA}/5`);
  expect(store.torrents[0].files![0].priority).toBe(1);
  receive([fixture(-1)], `${epochA}/4`);
  expect(store.torrents[0].files![0].priority).toBe(1);
});

it('accepts newer legacy state without waiting for a matching local priority', async () => {
  const store = useTorrentStore(); store.torrents = [fixture()]; store.initSSE();
  vi.mocked(apiClient.setFilePriority).mockResolvedValueOnce(100);
  await store.setFilePriority('one', 0, -1);
  vi.mocked(setupSSE).mock.calls[0][0]([{ ...fixture(1), snapshot_at_ns: 110 }]);
  expect(store.torrents[0].files![0].priority).toBe(1);
});

it('fences an in-flight details response across a priority receipt', async () => {
  const store = useTorrentStore(); store.initSSE();
  const receive = vi.mocked(setupSSE).mock.calls[0][0];
  receive([{ ...fixture(), files: undefined }], `${epochA}/1`);
  let resolve!: (files: NonNullable<Torrent['files']>) => void;
  vi.mocked(apiClient.getFiles).mockReturnValueOnce(new Promise(r => { resolve = r; }));
  const load = store.selectTorrent('one');
  vi.mocked(apiClient.setFilePriority).mockResolvedValueOnce({ commandAtNS: 100, snapshotVersion: `${epochA}/3` });
  await store.setFilePriority('one', 0, -1);
  resolve(fixture().files!); await load;
  expect(store.files.one).toBeUndefined();
  vi.mocked(apiClient.getFiles).mockResolvedValueOnce(fixture(1).files!);
  receive([{ ...fixture(), files: undefined }], `${epochA}/5`); await flushPromises();
  expect(store.files.one[0].priority).toBe(1);
});
