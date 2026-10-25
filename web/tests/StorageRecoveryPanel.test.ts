import { afterEach, describe, expect, it, vi } from 'vitest';
import { mount, flushPromises } from '@vue/test-utils';
import { createPinia, setActivePinia } from 'pinia';
import StorageRecoveryPanel from '../src/components/StorageRecoveryPanel.vue';
import TorrentTable from '../src/components/TorrentTable.vue';
import { useTorrentStore } from '../src/stores/torrentStore';
import type { Torrent } from '../src/types';

const fixture = () => ({ hash: 'one', name: 'movie', state: 'Paused', storage_state: 'ready', metadata_known: true,
  size: 8, progress: 0, download_speed: 0, upload_speed: 0, is_temporary: false,
  storage_recovery: { record_id: 'record', version: '2', reason: 'deletion_pending', runtime: 'absent', data_readiness: 'unknown',
    actions: ['retry_deletion', 'cancel_deletion'], deletion: { mode: 'payload', stage: 'removing', removed_data: 'partial' } },
}) as Torrent;
let wrapper: ReturnType<typeof mount> | undefined;
afterEach(() => { wrapper?.unmount(); document.body.innerHTML = ''; });

describe('storage recovery controls', () => {
  it('requires cancellation confirmation and explains unrecoverable deleted files', async () => {
    const pinia = createPinia(); setActivePinia(pinia); const store = useTorrentStore();
    const torrent = fixture(); store.torrents = [torrent];
    // Cancellation must remain usable while this tab's original Delete waits.
    store.actionPending.one = true;
    const recover = vi.spyOn(store, 'recoverStorage').mockResolvedValue(true);
    wrapper = mount(StorageRecoveryPanel, { props: { torrent }, attachTo: document.body, global: { plugins: [pinia] } });
    const cancel = wrapper.findAll('button').find(b => b.text() === 'Cancel further deletion')!;
    expect(cancel.attributes('disabled')).toBeUndefined();
    await cancel.trigger('click');
    expect(recover).not.toHaveBeenCalled();
    expect(document.body.textContent).toContain('Already removed files cannot be restored');
    const confirm = Array.from(document.body.querySelectorAll('button')).find(b => b.textContent?.trim() === 'Confirm')!;
    confirm.click(); await flushPromises();
    expect(recover).toHaveBeenCalledWith('one', 'cancel_deletion', undefined);
  });

  it('does not show ordinary resume or opposite delete for pending deletion', async () => {
    const pinia = createPinia(); setActivePinia(pinia); const store = useTorrentStore(); store.torrents = [fixture()];
    wrapper = mount(TorrentTable, { attachTo: document.body, global: { plugins: [pinia] } });
    const menu = wrapper.findAll('tbody button').find(b => b.find('svg').exists())!;
    await menu.trigger('click'); await flushPromises();
    expect(document.body.textContent).not.toContain('Remove with files');
    expect(document.body.textContent).not.toContain('metadata pending');
    expect(wrapper.text()).toContain('Layout: ready');
    expect(wrapper.text()).toContain('Removed data: partial');
  });

  it('invalidates a pending candidate confirmation when the operation changes', async () => {
    const pinia = createPinia(); setActivePinia(pinia); const store = useTorrentStore();
    const torrent = fixture(); torrent.storage_recovery!.reason = 'legacy_file_conflict'; torrent.storage_recovery!.actions = ['apply_legacy'];
    const recover = vi.spyOn(store, 'recoverStorage').mockResolvedValue(true);
    wrapper = mount(StorageRecoveryPanel, { props: { torrent }, attachTo: document.body, global: { plugins: [pinia] } });
    await wrapper.findAll('button').find(b => b.text().startsWith('Use .part'))!.trigger('click');
    expect(document.body.textContent).toContain('No files are merged');
    await wrapper.setProps({ torrent: { ...torrent, storage_recovery: { ...torrent.storage_recovery!, version: '3' } } });
    expect(document.body.querySelector('[role="dialog"]')).toBeNull();
    expect(recover).not.toHaveBeenCalled();
  });
});
