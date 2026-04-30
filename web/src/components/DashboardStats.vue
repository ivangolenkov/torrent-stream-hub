<script setup lang="ts">
import { computed } from 'vue';
import { useTorrentStore } from '../stores/torrentStore';

const store = useTorrentStore();

const formatSpeed = (bytesPerSec: number) => {
  if (!bytesPerSec) return '0 B/s';
  const k = 1024;
  const sizes = ['B/s', 'KB/s', 'MB/s', 'GB/s'];
  const i = Math.floor(Math.log(bytesPerSec) / Math.log(k));
  return parseFloat((bytesPerSec / Math.pow(k, i)).toFixed(2)) + ' ' + sizes[i];
};

const stats = computed(() => {
  let down = 0;
  let up = 0;
  let active = 0;
  
  store.torrents.forEach(t => {
    down += t.download_speed || 0;
    up += t.upload_speed || 0;
    if (['Downloading', 'Streaming', 'Seeding'].includes(t.state)) {
      active++;
    }
  });

  return { down, up, active, total: store.torrents.length };
});
</script>

<template>
  <div class="grid grid-cols-1 gap-5 sm:grid-cols-4">
    <div class="bg-white overflow-hidden shadow rounded-lg border border-gray-200">
      <div class="px-4 py-5 sm:p-6">
        <dt class="text-sm font-medium text-gray-500 truncate">Total Downloads</dt>
        <dd class="mt-1 text-3xl font-semibold text-gray-900">{{ stats.total }}</dd>
      </div>
    </div>
    <div class="bg-white overflow-hidden shadow rounded-lg border border-gray-200">
      <div class="px-4 py-5 sm:p-6">
        <dt class="text-sm font-medium text-gray-500 truncate">Active Torrents</dt>
        <dd class="mt-1 text-3xl font-semibold text-blue-600">{{ stats.active }}</dd>
      </div>
    </div>
    <div class="bg-white overflow-hidden shadow rounded-lg border border-gray-200">
      <div class="px-4 py-5 sm:p-6">
        <dt class="text-sm font-medium text-gray-500 truncate">Global Download</dt>
        <dd class="mt-1 text-3xl font-semibold text-green-600">{{ formatSpeed(stats.down) }}</dd>
      </div>
    </div>
    <div class="bg-white overflow-hidden shadow rounded-lg border border-gray-200">
      <div class="px-4 py-5 sm:p-6">
        <dt class="text-sm font-medium text-gray-500 truncate">Global Upload</dt>
        <dd class="mt-1 text-3xl font-semibold text-indigo-600">{{ formatSpeed(stats.up) }}</dd>
      </div>
    </div>
  </div>
</template>
