import { afterEach, expect, it, vi } from 'vitest';
import { mount, flushPromises } from '@vue/test-utils';
import { createPinia, setActivePinia } from 'pinia';
import { nextTick } from 'vue';
import BottomPanel from '../src/components/BottomPanel.vue';
import { useTorrentStore } from '../src/stores/torrentStore';
import type { Torrent } from '../src/types';

afterEach(() => { vi.useRealTimers(); vi.unstubAllGlobals(); });

it('aborts polling on switch/unmount and rejects late responses', async () => {
  vi.useFakeTimers();
  const pending: Array<{ resolve: (value: unknown) => void; signal: AbortSignal }> = [];
  const fetchMock = vi.fn((_url: string, options: RequestInit) => new Promise(resolve => pending.push({ resolve, signal: options.signal as AbortSignal })));
  vi.stubGlobal('fetch', fetchMock);
  const pinia = createPinia(); setActivePinia(pinia);
  const store = useTorrentStore();
  store.torrents = ['one', 'two'].map(hash => ({ hash, name: hash, size: 10, files: [{ index: 0, size: 10, downloaded: 0, path: 'video', offset: 0 }], peer_summary: {} } as Torrent));
  store.selectedHash = 'one';
  const wrapper = mount(BottomPanel, { global: { plugins: [pinia], stubs: { PieceProgressBar: true, VideoPlayer: true, TemporaryTorrentControl: true } } });
  await wrapper.findAll('button').find(b => b.text() === 'Files')!.trigger('click');
  expect(fetchMock).toHaveBeenCalledTimes(1);
  await vi.advanceTimersByTimeAsync(6000);
  expect(fetchMock).toHaveBeenCalledTimes(1); // No overlapping requests.
  store.selectedHash = 'two'; await nextTick();
  expect(pending[0].signal.aborted).toBe(true);
  expect(fetchMock).toHaveBeenCalledTimes(2);
  pending[1].resolve({ ok: true, text: async () => '222' }); await flushPromises();
  pending[0].resolve({ ok: true, text: async () => '111' }); await flushPromises();
  expect(wrapper.findComponent({ name: 'PieceProgressBar' }).props('piecesString')).toBe('222');
  await vi.advanceTimersByTimeAsync(2000);
  expect(fetchMock).toHaveBeenCalledTimes(3);
  wrapper.unmount();
  expect(pending[2].signal.aborted).toBe(true);
  await vi.advanceTimersByTimeAsync(10000);
  expect(fetchMock).toHaveBeenCalledTimes(3);
});
