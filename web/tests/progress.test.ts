import { expect, it } from 'vitest';
import { fileProgress, progressLabel } from '../src/progress';
import type { Torrent } from '../src/types';

it('keeps empty and invalid file percentages finite', () => {
  expect(fileProgress(0, 0)).toBe(0);
  expect(fileProgress(5, 0)).toBe(0);
  expect(fileProgress(-1, 10)).toBe(0);
  expect(fileProgress(11, 10)).toBe(100);
  expect(fileProgress(Infinity, 10)).toBe(0);
});

it('labels stored observations without claiming current verification', () => {
  expect(progressLabel({ progress_source: 'last_known' } as Torrent)).toContain('verification required');
  expect(progressLabel({ progress_source: 'legacy' } as Torrent)).toContain('Legacy estimate');
  expect(progressLabel({ progress_source: 'checking' } as Torrent)).toContain('in progress');
  expect(progressLabel({ progress_source: 'verified' } as Torrent)).toBe('');
});
