import { mount } from '@vue/test-utils';
import { describe, expect, it } from 'vitest';
import ResumeObservation from '../src/components/ResumeObservation.vue';
import fixture from '../../internal/models/testdata/resume.json';
import type { ResumeProgress, Torrent } from '../src/types';

describe('resume diagnosis', () => {
  it('uses shared wire fixture and never rounds int64 generations', () => {
    const resume = fixture as ResumeProgress;
    const wrapper = mount(ResumeObservation, { props: { torrent: { resume } as Torrent } });
    expect(wrapper.text()).toContain('9007199254740993');
    expect(wrapper.text()).toContain('Restored from saved state');
    expect(wrapper.text()).toContain('Hash-verified in this attachment');
    expect(wrapper.text()).toContain('Same-size external changes');
    expect(wrapper.text()).toContain('does not resume a paused torrent');
  });
  it('explains fallback without implying deleted data', () => {
    const resume = { ...fixture, status: 'fallback', reason: 'no_checkpoint' } as ResumeProgress;
    const wrapper = mount(ResumeObservation, { props: { torrent: { resume } as Torrent } });
    expect(wrapper.text()).toContain('Downloaded files are not lost');
  });
  it('allows old servers without diagnosis', () => {
    expect(mount(ResumeObservation, { props: { torrent: {} as Torrent } }).text()).toBe('');
  });
});
