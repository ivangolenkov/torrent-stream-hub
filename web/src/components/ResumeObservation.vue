<script setup lang="ts">
import type { Torrent } from '../types';
defineProps<{ torrent: Torrent }>();
const reasons: Record<string, string> = {
  restored: 'Saved verified pieces restored without hashing.',
  strict_mode: 'Strict mode: hash-check data on every open.',
  no_checkpoint: 'No usable checkpoint yet; ordinary verification is required. Downloaded files are not lost.',
  invalid_checkpoint: 'Incompatible checkpoint; using ordinary verification.',
  budget_exceeded: 'Checkpoint exceeds safe memory limits; using ordinary verification.',
  files_changed: 'File evidence changed; affected pieces require verification.',
  pieces_revoked: 'Previously changed pieces require verification; independent saved pieces may be restored.',
  storage_blocked: 'Storage safety gates prevent trusted restore.',
  identity_changed: 'Storage identity changed; using ordinary verification.',
  checkpoint_io_error: 'Checkpoint could not be read; using ordinary verification.',
  checkpoint_integrity_error: 'Damaged checkpoint; using ordinary verification.',
  unsupported_storage: 'Trusted restore is unavailable for this storage.',
};
</script>

<template>
  <details v-if="torrent.resume" class="text-xs text-gray-600">
    <summary>Resume: {{ torrent.resume.mode }} · {{ torrent.resume.status }}</summary>
    <p>{{ reasons[torrent.resume.reason] || 'Waiting for restore diagnosis.' }}</p>
    <p>Restored from saved state: {{ torrent.resume.restored_bytes.toLocaleString() }} B · Hash-verified in this attachment: {{ torrent.resume.freshly_verified_bytes.toLocaleString() }} B</p>
    <p v-if="torrent.resume.checkpoint_generation">Checkpoint {{ torrent.resume.checkpoint_generation }}<span v-if="torrent.resume.checkpoint_at_ms"> · {{ new Date(torrent.resume.checkpoint_at_ms).toLocaleString() }}</span></p>
    <p v-if="torrent.resume.mode === 'fast'">Fast mode assumes only this service writes payload. Same-size external changes with restored timestamps may go undetected. After editing, sync or backup restore, use Recheck in the torrent menu (does not resume a paused torrent). For strict checking on every open set HUB_FAST_RESUME_MODE=verify and restart.</p>
  </details>
</template>
