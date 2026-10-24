import { expect, it, vi } from 'vitest';
import { mount, flushPromises } from '@vue/test-utils';
import BTHealthPanel from '../src/components/BTHealthPanel.vue';
import { apiClient } from '../src/api/client';
import type { BTHealth } from '../src/types';

vi.mock('../src/api/client', () => ({ apiClient: { getBTHealth: vi.fn() } }));

it('shows actual boost and last-known diagnostics without unsupported commands', async () => {
  // An elapsed deadline remains boosted until the engine restores its limit.
  const fixture = { incoming_connectivity_note: 'LAN', piece_completion_backend: 'sqlite', piece_completion_pending: 2, piece_completion_dropped: 3,
    torrents: [{ hash: 'one', name: 'video', degraded: false, boosted_until: '2000-01-01T00:00:00Z', max_established_conns: 400 }] } as BTHealth;
  vi.mocked(apiClient.getBTHealth).mockResolvedValueOnce(fixture);
  const wrapper = mount(BTHealthPanel);
  try {
    await flushPromises();
    expect(wrapper.text()).toContain('Boosted');
    expect(wrapper.text()).toContain('400 connections');
    expect(wrapper.text()).toContain('pending 2 · dropped 3');
    expect(wrapper.findAll('button').map(b => b.text())).toEqual(['Reload diagnostics']);
    vi.mocked(apiClient.getBTHealth).mockRejectedValueOnce(new Error('Server unavailable'));
    await wrapper.find('button').trigger('click'); await flushPromises();
    expect(wrapper.text()).toContain('Showing last received diagnostics');
    expect(wrapper.text()).toContain('video');
  } finally { wrapper.unmount(); }
});
