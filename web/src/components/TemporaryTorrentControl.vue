<script setup lang="ts">
import type { Torrent } from '../types';
import { useTorrentStore } from '../stores/torrentStore';

defineProps<{ torrent: Torrent }>();
const store = useTorrentStore();
</script>

<template>
  <div v-if="torrent.is_temporary" class="mt-1 text-xs whitespace-normal" @click.stop>
    <span class="inline-block rounded bg-amber-100 px-2 py-0.5 text-amber-800"
          title="Временный торрент может быть автоматически удалён вместе с файлами">
      Временный
    </span>
    <button class="ml-2 text-blue-600 hover:underline disabled:opacity-50"
            :disabled="store.pinning[torrent.hash]"
            @click="store.makePermanent(torrent.hash)">
      {{ store.pinning[torrent.hash] ? 'Сохранение…' : 'Сделать постоянным' }}
    </button>
    <p class="mt-1 text-gray-500">Может быть автоматически удалён вместе с файлами.</p>
    <p v-if="store.pinErrors[torrent.hash]" role="alert" class="mt-1 text-red-600">{{ store.pinErrors[torrent.hash] }}</p>
  </div>
</template>
