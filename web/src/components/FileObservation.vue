<script setup lang="ts">
import type { File } from '../types';
import { fileProgress } from '../progress';
defineProps<{ file: File }>();
const reasonLabels: Record<string, string> = {
  runtime_not_attached: 'Waiting for local verification', verification_required: 'Verification required',
  incomplete: 'File is incomplete',
  inventory_pending: 'Storage inspection pending', expected_file_missing: 'Previously present file is missing',
  not_created: 'File has not been created', volume_unavailable: 'Storage volume unavailable',
  volume_unregistered: 'Storage needs confirmation', access_error: 'Cannot access file', metadata_error: 'Cannot read saved metadata',
  storage_operation: 'File operation in progress', storage_unavailable: 'Storage unavailable',
  verification: 'Local verification required', missing_files: 'Missing files require recovery', deleting: 'Deletion pending',
};
</script>

<template>
  <div v-if="file.status" class="mt-1 space-y-0.5 whitespace-normal text-xs text-gray-500" aria-label="File status">
    <p class="break-all">Metadata: {{ file.metadata_path || file.path }}</p>
    <p v-if="file.status.storage_path" class="break-all">Location: {{ file.status.storage_path }}</p>
    <p>{{ file.status.presence }}<span v-if="file.status.observed_at_ms"> · observed {{ new Date(file.status.observed_at_ms).toLocaleTimeString() }}</span></p>
    <p>Verified now: {{ file.size ? `${fileProgress(file.status.verified_bytes, file.size).toFixed(1)}%` : 'empty file' }} · Check: {{ file.status.verification }}</p>
    <p v-if="file.status.last_known_source && file.status.last_known_source !== 'unknown'">
      Last known: {{ file.size ? `${fileProgress(file.status.last_known_bytes, file.size).toFixed(1)}%` : 'empty file' }} ({{ file.status.last_known_source }})
      <span v-if="file.status.last_known_at_ms"> · {{ new Date(file.status.last_known_at_ms).toLocaleString() }}</span>
    </p>
    <p v-if="file.status.direct_read_ready">Ready for file download</p>
    <p v-else>{{ reasonLabels[file.status.read_reason || ''] || file.status.read_reason || 'File is incomplete' }}</p>
    <p v-if="file.status.reason && file.status.reason !== file.status.read_reason">{{ reasonLabels[file.status.reason] || file.status.reason }}</p>
    <p>Effective priority: {{ file.status.effective_priority }}</p>
  </div>
</template>
