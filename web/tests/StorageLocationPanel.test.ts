import { afterEach, describe, expect, it, vi } from 'vitest';
import { mount, flushPromises } from '@vue/test-utils';
import { createPinia, setActivePinia } from 'pinia';
import StorageLocationPanel from '../src/components/StorageLocationPanel.vue';
import { apiClient } from '../src/api/client';
import type { Torrent } from '../src/types';

const fixture = () => ({ hash: 'one', metadata_known: true,
  control: { record_id: 'record' }, storage: { id: 'storage', revision: '9007199254740993', layout_version: 2,
    root: 'payload/Original', prefix: 'Original', status: 'ready', operation: { kind: 'move', stage: 'publishing',
      version: '5', source: 'payload/Original', target: 'payload/Moved', actions: ['retry_storage'] } },
}) as Torrent;
afterEach(() => vi.restoreAllMocks());

describe('storage location controls', () => {
  it('honors publication actions and fences errors after switching records', async () => {
    const pinia = createPinia(); setActivePinia(pinia);
    let reject!: (cause: Error) => void;
    const send = vi.spyOn(apiClient, 'storageCommand').mockImplementation(() => new Promise((_, r) => { reject = r; }));
    const wrapper = mount(StorageLocationPanel, { props: { torrent: fixture() }, global: { plugins: [pinia] } });
    expect(wrapper.text()).not.toContain('Cancel operation');
    await wrapper.find('button').trigger('click');
    expect(send).toHaveBeenCalledWith('one', expect.objectContaining({ action: 'retry_storage', record_id: 'record', mapping_revision: '9007199254740993', version: '5' }));
    const next = fixture(); next.hash = 'two'; next.control!.record_id = 'new-record';
    await wrapper.setProps({ torrent: next });
    reject(new Error('old operation failed')); await flushPromises();
    expect(wrapper.text()).not.toContain('old operation failed');
    expect(wrapper.find('button').attributes('disabled')).toBeUndefined();
    wrapper.unmount();
  });
});
