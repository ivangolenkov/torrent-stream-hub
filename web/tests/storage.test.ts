import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { expect, it } from 'vitest';
import type { FileStatus, SelectionProgress, StorageCommand, StorageLocation } from '../src/types/storage';

const data = JSON.parse(readFileSync(resolve(process.cwd(), '../internal/models/testdata/storage-location.json'), 'utf8')) as {
  storage: StorageLocation; file: FileStatus; selection: SelectionProgress; empty_selection: SelectionProgress; command: StorageCommand;
};
it('keeps storage identity, operation and snapshot concepts separate without numeric revision rounding', () => {
  const command: StorageCommand = { action: 'apply_move', record_id: 'record-a', storage_id: 'storage-a', mapping_revision: '9007199254740993', version: '4' };
  expect(data.command).toEqual(command);
  expect(data.storage.revision).toBe(command.mapping_revision);
  expect(data.storage.operation?.version).toBe(command.version);
});
it('distinguishes historical data from readiness and empty selection from completed selection', () => {
  expect(data.file.last_known_bytes).toBe(16);
  expect(data.file.verified_bytes).toBe(0);
  expect(data.file.direct_read_ready).toBe(false);
  expect(data.empty_selection.progress).toBeUndefined();
  expect(data.selection.progress).toBe(100);
});
