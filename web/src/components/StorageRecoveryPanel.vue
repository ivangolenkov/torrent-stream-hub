<script setup lang="ts">
import { computed, ref, watch } from 'vue';
import type { Torrent } from '../types';
import type { StorageRecoveryAction } from '../types/storageRecovery';
import { useTorrentStore } from '../stores/torrentStore';
import ConfirmDialog from './ConfirmDialog.vue';

const props = defineProps<{ torrent: Torrent }>();
const store = useTorrentStore();
const recovery = computed(() => props.torrent.storage_recovery);
const pending = ref<{ action: StorageRecoveryAction; choice?: 'final' | 'part'; id: string; version: string } | null>(null);
const labels: Record<StorageRecoveryAction, string> = {
  retry_deletion: 'Retry original deletion', cancel_deletion: 'Cancel further deletion',
  forget_record: 'Remove record, keep remaining files', resume_verify: 'Verify remaining data and resume',
  analyze_legacy: 'Analyze both candidates', apply_legacy: 'Apply selected candidate', retry_ownership: 'Retry ownership check',
};
const reasons: Record<string, string> = {
  storage_move: 'Moving files', storage_locate: 'Finding existing data',
  legacy_file_conflict: 'Legacy file conflict — both copies preserved',
  deletion_pending: 'Deletion pending', deletion_cancelled: 'Deletion cancelled — remaining data not yet verified',
  deletion_mode_conflict: 'Deletion mode differs from the saved operation',
  ownership_catalog_changed: 'Ownership must be checked again', migration_queued: 'Storage migration queued',
  ownership_conflict: 'Storage ownership is unconfirmed — paths may overlap another record',
  migration_active: 'Storage migration in progress', attachment_failed: 'Runtime attachment failed',
  disk_full: 'Insufficient free space — recovery required', recovery_complete: 'Storage recovery completed — alternatives retained',
};
const busy = computed(() => !!store.recoveryPending[props.torrent.hash] || (!!recovery.value?.progress && recovery.value.progress.stage !== 'queued'));
const request = async (action: StorageRecoveryAction, choice?: 'final' | 'part') => {
  const s = recovery.value;
  if (!s) return;
  if (['cancel_deletion', 'forget_record', 'apply_legacy', 'resume_verify'].includes(action)) {
    pending.value = { action, choice, id: s.record_id, version: s.version };
  } else await store.recoverStorage(props.torrent.hash, action, choice);
};
const confirm = async () => {
  const p = pending.value; const s = recovery.value;
  if (!p || !s || p.id !== s.record_id || p.version !== s.version) { pending.value = null; return; }
  if (await store.recoverStorage(props.torrent.hash, p.action, p.choice)) pending.value = null;
};
watch(() => `${recovery.value?.record_id}/${recovery.value?.version}`, () => { pending.value = null; });
const confirmation = computed(() => pending.value?.action === 'cancel_deletion'
  ? 'Stop further deletion and wait for the current filesystem operation. Already removed files cannot be restored. Remaining data may be partial or unknown. The torrent stays paused until explicit verification.'
  : pending.value?.action === 'apply_legacy'
    ? `Use ${pending.value.choice === 'part' ? 'the legacy .part candidate' : 'the metadata-name candidate'}? The alternative is retained in protected recovery storage on the same disk. No files are merged. Verification is still required before runtime access.`
    : pending.value?.action === 'forget_record' ? 'Remove only the record. Keep all remaining payload and protected recovery copies.'
      : 'Verify remaining data locally, then explicitly resume downloading. This does not restore deleted files or clear a disk-space safety block.');
const bytes = (n?: number) => n === undefined ? 'unknown' : `${n.toLocaleString()} B`;
</script>

<template>
  <section v-if="recovery" class="text-xs text-amber-800 whitespace-normal space-y-1" @click.stop aria-label="Storage recovery">
    <div>{{ reasons[recovery.reason] || recovery.reason }}</div>
    <div v-if="recovery.repeat_count">Repeated failures: {{ recovery.repeat_count }}</div>
    <div>Layout: {{ torrent.storage_state || 'unknown' }} · Runtime: {{ recovery.runtime }} · Data: {{ recovery.data_readiness }}</div>
    <div v-if="recovery.deletion">Mode: {{ recovery.deletion.mode }} · {{ recovery.deletion.stage }} · Removed data: {{ recovery.deletion.removed_data }}. {{ recovery.deletion.error }}</div>
    <div v-if="torrent.storage_error" role="status">{{ torrent.storage_error }} <span v-if="recovery.failure_code">({{ recovery.failure_code }})</span></div>
    <div v-if="recovery.progress">
      <div>Stage: {{ recovery.progress.stage }} <span v-if="recovery.progress.queue_position">· Queue: {{ recovery.progress.queue_position }}</span></div>
      <div>{{ bytes(recovery.progress.bytes_done) }} / {{ bytes(recovery.progress.bytes_total) }} · Files: {{ recovery.progress.files_done }} / {{ recovery.progress.files_total ?? '?' }}</div>
      <div v-if="recovery.progress.file_path">{{ recovery.progress.file_path }}: {{ bytes(recovery.progress.file_bytes_done) }} / {{ bytes(recovery.progress.file_bytes_total) }}</div>
      <div>Last byte/file progress: {{ recovery.progress.last_progress_at_ms ? new Date(recovery.progress.last_progress_at_ms).toLocaleTimeString() : 'not started' }} · Heartbeat: {{ new Date(recovery.progress.heartbeat_at_ms).toLocaleTimeString() }}</div>
    </div>
    <div v-if="recovery.analysis">
      <div>{{ recovery.analysis.path }} · {{ recovery.analysis.equal ? 'Identical copies' : 'Different copies' }} · Suggested: {{ recovery.analysis.recommended || 'ambiguous — choose explicitly' }}</div>
      <div>Metadata file: {{ recovery.analysis.final.valid_pieces }}/{{ recovery.analysis.final.total_pieces }} valid boundary pieces · .part: {{ recovery.analysis.part.valid_pieces }}/{{ recovery.analysis.part.total_pieces }}</div>
      <div v-for="limitation in recovery.analysis.limitations" :key="limitation">{{ limitation }}</div>
    </div>
    <div v-if="recovery.alternative_copies">Protected alternatives: {{ recovery.alternative_copies }} · {{ bytes(recovery.alternative_bytes) }}. Not removed by Resume or torrent cleanup.</div>
    <div class="flex flex-wrap gap-2">
      <button v-for="action in recovery.actions.filter(a => a !== 'apply_legacy')" :key="action" :disabled="busy" class="underline disabled:opacity-50" @click="request(action)">{{ labels[action] }}</button>
      <template v-if="recovery.actions.includes('apply_legacy')">
        <button :disabled="busy" class="underline disabled:opacity-50" @click="request('apply_legacy', 'final')">Use metadata file (retain .part)</button>
        <button :disabled="busy" class="underline disabled:opacity-50" @click="request('apply_legacy', 'part')">Use .part (retain metadata file)</button>
      </template>
    </div>
    <ConfirmDialog :is-open="!!pending" title="Storage recovery" :message="confirmation" confirm-label="Confirm" :loading="!!store.recoveryPending[torrent.hash]" @cancel="pending = null" @confirm="confirm" />
  </section>
</template>
