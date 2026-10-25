import { describe, expect, it } from 'vitest';
import { mount } from '@vue/test-utils';
import FileObservation from '../src/components/FileObservation.vue';
import { selectionLabel, activityLabel, progressLabel } from '../src/progress';
import type { File, Torrent } from '../src/types';

describe('separate file observations', () => {
  it('shows current and historical bytes while checking without granting download readiness', () => {
    const file = { path: 'a', metadata_path: 'Original/a', size: 4, downloaded: 0, priority: -1,
      status: { storage_path: 'payload/Readable/a', presence: 'present', verified_bytes: 0, last_known_bytes: 4,
        last_known_source: 'last_known', verification: 'running', direct_read_ready: false, read_reason: 'verification', effective_priority: 'none' } } as File;
    const wrapper = mount(FileObservation, { props: { file } });
    expect(wrapper.text()).toContain('Verified now: 0.0%');
    expect(wrapper.text()).toContain('Last known: 100.0%');
    expect(wrapper.text()).toContain('payload/Readable/a');
    expect(wrapper.text()).not.toContain('Ready for file download');
  });
  it('keeps selected progress independent and does not diagnose peers from zero speed', () => {
    const torrent = { metadata_known: true, size: 8, progress: 50, state: 'Queued', selection: { files: 1, size: 4, verified_bytes: 4, progress: 100, complete: true } } as Torrent;
    expect(selectionLabel(torrent)).toContain('Selected: 100.0%');
    expect(selectionLabel(torrent)).toContain('whole torrent: 50.0%');
    expect(activityLabel(torrent)).toBe('');
    torrent.selection = { files: 0, size: 0, verified_bytes: 0 };
    expect(selectionLabel(torrent)).toBe('No selected files');
    torrent.state='Downloading'; torrent.download_speed=0;
    expect(activityLabel(torrent)).toBe('No transfer');
    torrent.progress_source='checking'; torrent.last_known_bytes=8; torrent.last_known_source='last_known';
    expect(progressLabel(torrent)).toContain('last known: 100.0%');
  });
});
