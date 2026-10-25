<script setup lang="ts">
import { computed, ref, watch } from 'vue';
import type { Torrent } from '../types';
import type { StorageCommand } from '../types/storage';
import { apiClient } from '../api/client';
import { useTorrentStore } from '../stores/torrentStore';

const props = defineProps<{ torrent: Torrent }>();
const store = useTorrentStore();
const folder = ref('');
const candidate = ref('');
const prefix = ref('');
const busy = ref(false);
const error = ref('');
const op = computed(() => props.torrent.storage?.operation);
const canPlan = computed(() => props.torrent.storage?.status === 'ready' && props.torrent.metadata_known &&
  !props.torrent.deletion_mode && (!op.value || ['planned', 'analyzed', 'completed', 'cancelled'].includes(op.value.stage)));
const labels: Record<string, string> = { apply_move: 'Move files', apply_location: 'Use these files and pause', retry_storage: 'Retry', cancel_storage: 'Cancel operation' };
const stages: Record<string, string> = { analyzing_location: 'Checking existing data', analyzed: 'Analysis ready', planning: 'Inspecting files', planned: 'Plan ready', queued: 'Waiting to move', waiting_readers: 'Waiting for active transfers', prepared: 'Preparing files', copying: 'Copying files', publishing: 'Publishing the new location', published: 'Saving the new location', committed: 'Cleaning up old copies', cleanup: 'Cleaning up old copies', completed: 'Move completed', cancel_requested: 'Cancelling', cancelled: 'Move cancelled' };
watch(() => [props.torrent.hash, props.torrent.control?.record_id], () => { folder.value = ''; candidate.value = ''; prefix.value = ''; error.value = ''; busy.value = false; });

async function command(action: StorageCommand['action']) {
  const torrent = props.torrent;
  if (!torrent.storage || !torrent.control || busy.value) return;
  const hash = torrent.hash, record = torrent.control.record_id;
  busy.value = true; error.value = '';
  try {
    await apiClient.storageCommand(hash, {
      action, record_id: record, storage_id: torrent.storage.id,
      mapping_revision: torrent.storage.revision, version: torrent.storage.operation?.version,
      folder_name: folder.value, path: candidate.value || undefined, strip_prefix: prefix.value || undefined,
    });
    await store.loadTorrents();
  } catch (cause) {
    if (props.torrent.hash === hash && props.torrent.control?.record_id === record) error.value = cause instanceof Error ? cause.message : 'Storage operation failed';
  } finally {
    if (props.torrent.hash === hash && props.torrent.control?.record_id === record) busy.value = false;
  }
}
</script>

<template>
  <section v-if="torrent.storage" class="mb-6 rounded-lg border border-gray-200 bg-gray-50 p-4 text-sm text-gray-700" aria-label="File location">
    <h4 class="mb-2 font-semibold">File location</h4>
    <p class="break-all font-mono text-xs">{{ torrent.storage.root || (torrent.storage.status === 'reserved' ? 'Waiting for torrent metadata' : 'Files are in the shared download directory') }}</p>
    <p v-if="torrent.storage.reason" class="mt-2 text-amber-700">{{ torrent.storage.reason }}</p>
    <form v-if="canPlan" class="mt-3 flex flex-wrap gap-2" @submit.prevent="command('plan_move')">
      <label class="sr-only" :for="`move-folder-${torrent.hash}`">New folder name</label>
      <input :id="`move-folder-${torrent.hash}`" v-model="folder" class="min-w-0 flex-1 rounded border border-gray-300 bg-white px-2 py-1" placeholder="New folder name" maxlength="180">
      <button :disabled="busy" class="rounded border border-gray-300 bg-white px-3 py-1 disabled:opacity-50">Preview move</button>
    </form>
    <form v-if="canPlan" class="mt-3 flex flex-wrap gap-2" @submit.prevent="command('analyze_location')">
      <label class="sr-only" :for="`locate-path-${torrent.hash}`">Existing directory relative to downloads</label>
      <input :id="`locate-path-${torrent.hash}`" v-model="candidate" class="rounded border border-gray-300 bg-white px-2 py-1" placeholder="Existing directory in downloads" required>
      <label class="sr-only" :for="`locate-prefix-${torrent.hash}`">Metadata root to omit</label>
      <input :id="`locate-prefix-${torrent.hash}`" v-model="prefix" class="rounded border border-gray-300 bg-white px-2 py-1" placeholder="Outer torrent folder already omitted? Enter its name">
      <button :disabled="busy" class="rounded border border-gray-300 bg-white px-3 py-1">Analyze existing files</button>
    </form>
    <div v-if="torrent.storage.inventory && torrent.storage.inventory !== 'ready'" class="mt-3" :class="torrent.storage.inventory === 'observing' ? 'text-gray-600' : 'text-amber-700'">
      <p>Storage inspection: {{ torrent.storage.inventory }} {{ torrent.storage.inventory_reason }}</p>
      <button v-if="!op || op.error || ['planned', 'analyzed', 'completed', 'cancelled'].includes(op.stage)" :disabled="busy" class="mt-2 rounded border px-3 py-1" @click="command('reconcile_storage')">Check storage and keep paused</button>
      <p class="mt-1 text-xs">A missing registered volume must be restored. This check does not start downloads.</p>
    </div>
    <div v-if="op" class="mt-3 space-y-2">
      <p class="font-medium">{{ op.kind === 'locate' && op.stage === 'completed' ? 'Existing location connected' : stages[op.stage] || op.stage }}</p>
      <p class="break-all text-xs">{{ op.source }} → {{ op.target }}</p>
      <p v-if="op.method" class="text-xs">{{ op.method === 'locate' ? 'Use existing files; preserve the previous location' : op.method === 'rename' ? 'Rename; copy if required by the filesystem' : 'Copy, verify, then remove original files' }}</p>
      <p v-if="op.extra_bytes !== undefined" class="text-xs">Additional space for a copy: {{ (op.extra_bytes / 1048576).toFixed(1) }} MiB</p>
      <p v-if="op.free_bytes !== undefined" class="text-xs">Free space when planned: {{ (op.free_bytes / 1048576).toFixed(1) }} MiB. Checked again before copying.</p>
      <div v-if="op.analysis" class="text-xs">
        <p>Found files: {{ op.analysis.found_files }}; missing: {{ op.analysis.missing_files }}. Verified pieces: {{ op.analysis.valid_pieces }}/{{ op.analysis.total_pieces }}.</p>
        <p>Applying keeps the torrent paused. Runtime verifies data again before reading.</p>
      </div>
      <p v-if="op.error" class="text-red-700" role="alert">{{ op.error }}</p>
      <div class="flex gap-2">
        <button v-for="action in op.actions.filter(action => action in labels)" :key="action" :disabled="busy" class="rounded border border-gray-300 bg-white px-3 py-1 disabled:opacity-50" @click="command(action as StorageCommand['action'])">{{ labels[action] }}</button>
      </div>
      <p v-if="op.stage === 'cancelled'" class="text-xs text-gray-500">Interrupted copies are preserved in recovery storage.</p>
    </div>
    <p v-if="error" class="mt-2 text-red-700" role="alert">{{ error }}</p>
  </section>
</template>
