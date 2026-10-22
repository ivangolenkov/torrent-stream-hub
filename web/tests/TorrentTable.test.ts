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
