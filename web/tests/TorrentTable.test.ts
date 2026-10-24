import { afterEach, describe, expect, it } from 'vitest';
import { mount, flushPromises } from '@vue/test-utils';
import { createPinia, setActivePinia } from 'pinia';
import { nextTick } from 'vue';
import TorrentTable from '../src/components/TorrentTable.vue';
import { useTorrentStore } from '../src/stores/torrentStore';
import type { Torrent } from '../src/types';

const fixture = (hash: string) => ({
  hash, name: hash, size: 1024, progress: 0, state: 'Paused',
  download_speed: 0, upload_speed: 0, peers: 0, is_temporary: false,
}) as Torrent;

const wrappers: ReturnType<typeof mount>[] = [];
afterEach(() => { wrappers.splice(0).forEach(w => w.unmount()); document.body.innerHTML = ''; });

function setup() {
  const pinia = createPinia();
  setActivePinia(pinia);
  const store = useTorrentStore();
  store.torrents = [fixture('one'), fixture('two')];
  const wrapper = mount(TorrentTable, { attachTo: document.body, global: { plugins: [pinia] } });
  wrappers.push(wrapper);
  return { wrapper, store };
}

describe('async action menu', () => {
  // Both templates render in jsdom; viewport/CSS visibility is not simulated.
  for (const [layout, selector] of [['desktop', 'tbody button'], ['mobile', '.md\\:hidden button']]) {
    it(`opens/closes from ${layout} using the captured dispatch target`, async () => {
      const { wrapper } = setup();
      const button = wrapper.find(selector);
      const event = new MouseEvent('click', { bubbles: true });
      button.element.dispatchEvent(event);
      expect(event.currentTarget).toBeNull();
      await flushPromises();
      expect(document.body.textContent).toContain('Remove with files');
      button.element.dispatchEvent(new MouseEvent('click', { bubbles: true }));
      await nextTick();
      expect(document.body.textContent).not.toContain('Remove with files');
    });
  }

  it('ignores stale opens, handles outside click and disappearing rows', async () => {
    const { wrapper, store } = setup();
    const buttons = wrapper.findAll('tbody button');
    buttons[0].element.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    buttons[1].element.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    await flushPromises();
    expect(document.body.textContent).toContain('Remove with files');
    document.body.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    await nextTick();
    expect(document.body.textContent).not.toContain('Remove with files');
    buttons[0].element.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    store.torrents = [];
    await flushPromises();
    expect(document.body.textContent).not.toContain('Remove with files');
  });
});

it('shows a persistent download block separately from manual pause in both layouts', async () => {
  const { wrapper, store } = setup();
  store.torrents = [{ ...fixture('one'), download_block: 'disk space unknown: probe failed' }];
  await nextTick();
  for (const selector of ['tbody', '.md\\:hidden']) {
    const text = wrapper.find(selector).text();
    expect(text).toContain('Paused');
    expect(text).toContain('disk space unknown: probe failed');
    expect(text).toContain('Resume manually after recovery');
  }
});

it('shows pending deletion and its failure in both layouts', async () => {
  const { wrapper, store } = setup();
  store.torrents = [{ ...fixture('one'), deletion_mode: 'keep', deletion_error: 'database commit failed' }];
  await nextTick();
  for (const selector of ['tbody', '.md\\:hidden']) {
    expect(wrapper.find(selector).text()).toContain('Deletion pending (keep)');
    expect(wrapper.find(selector).text()).toContain('database commit failed');
  }
});

it('labels last-known progress in both layouts', async () => {
  const { wrapper, store } = setup();
  store.torrents = [{ ...fixture('one'), progress: 60, progress_source: 'last_known' }];
  await nextTick();
  for (const selector of ['tbody', '.md\\:hidden']) {
    expect(wrapper.find(selector).text()).toContain('60.0%');
    expect(wrapper.find(selector).text()).toContain('Last known — verification required');
  }
});

it('explains migration waits without presenting them as missing peer metadata', async () => {
  const { wrapper, store } = setup();
  const torrent = {
    ...fixture('one'), storage_state: 'legacy', progress_source: 'legacy',
    peer_summary: { connected: 0, known: 0, seeds: 0, metadata_ready: false },
    control: { desired_run: 'enabled', reconcile_pending: true },
  } as Torrent;
  store.torrents = [torrent];
  await nextTick();
  for (const selector of ['tbody', '.md\\:hidden']) {
    const text = wrapper.find(selector).text();
    expect(text).toContain('Storage migration queued');
    expect(text).toContain('waiting for storage migration');
    expect(text).toContain('verification required');
    expect(text).not.toContain('metadata pending');
  }
  store.torrents = [{ ...torrent, storage_state: 'moving' }];
  await nextTick();
  expect(wrapper.text()).toContain('copying and verifying files');
  store.torrents = [{ ...torrent, storage_state: 'ready' }];
  await nextTick();
  expect(wrapper.text()).toContain('metadata pending');
  expect(wrapper.text()).toContain('apply pending');
  expect(wrapper.text()).not.toContain('waiting for storage migration');
});

it('shows streaming, preload and verification independently from Paused', async () => {
  const { wrapper, store } = setup();
  store.torrents = [{ ...fixture('one'), activity: { streaming: true, streams: 1, direct_readers: 1, preloads: 2, debouncing: false, verifying: true } }];
  await nextTick();
  for (const selector of ['tbody', '.md\\:hidden']) {
    const text = wrapper.find(selector).text();
    expect(text).toContain('Paused');
    expect(text).toContain('Streaming');
    expect(text).toContain('Preload: 2');
    expect(text).toContain('Verifying');
  }
});

it('shows action and recheck failures in both layouts', async () => {
  const { wrapper, store } = setup();
  store.torrents = [{ ...fixture('one'), recheck: { id: 1, status: 'failed', error: 'Read failed' } }];
  store.actionErrors.one = 'Priority commit failed';
  await nextTick();
  for (const selector of ['tbody', '.md\\:hidden']) {
    expect(wrapper.find(selector).text()).toContain('Priority commit failed');
    expect(wrapper.find(selector).text()).toContain('Recheck: failed Read failed');
  }
});

it('keeps committed intent, failed apply and independent disk recovery distinct', async () => {
  const { wrapper, store } = setup();
  store.torrents = [{ ...fixture('one'), download_block: 'disk latch', control: {
    record_id: 'record', desired_run: 'enabled', desired_revision: '9007199254740993',
    applied_revision: '0', reconcile_pending: true, reconcile_reason: 'runtime_apply_failed', activation_blocks: [],
    apply_failure: { record_id: 'record', revision: '9007199254740993', code: 'runtime_apply_failed', retry: 'explicit_pause_or_resume' },
  } }];
  await nextTick();
  for (const selector of ['tbody', '.md\\:hidden']) {
    const text = wrapper.find(selector).text();
    expect(text).toContain('Saved: enabled · apply pending');
    expect(text).toContain('Apply failed · retry with Pause or Resume');
    expect(text).toContain('disk latch');
    expect(text).toContain('Resume manually after recovery');
  }
});
