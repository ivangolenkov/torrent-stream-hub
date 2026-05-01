<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue';
import { apiClient } from '../api/client';
import type { BTHealth } from '../types';

const health = ref<BTHealth | null>(null);
const error = ref('');
let timer: number | undefined;

const formatSpeed = (bytesPerSec: number) => {
  if (!bytesPerSec) return '0 B/s';
  const k = 1024;
  const sizes = ['B/s', 'KB/s', 'MB/s', 'GB/s'];
  const i = Math.floor(Math.log(bytesPerSec) / Math.log(k));
  return `${parseFloat((bytesPerSec / Math.pow(k, i)).toFixed(2))} ${sizes[i]}`;
};

const loadHealth = async () => {
  try {
    health.value = await apiClient.getBTHealth();
    error.value = '';
  } catch (e) {
    error.value = e instanceof Error ? e.message : 'Failed to load BitTorrent health';
  }
};

const totals = computed(() => {
  const torrents = health.value?.torrents || [];
  return torrents.reduce(
    (acc, t) => {
      acc.known += t.known || 0;
      acc.connected += t.connected || 0;
      acc.pending += t.pending || 0;
      acc.halfOpen += t.half_open || 0;
      acc.seeds += t.seeds || 0;
      acc.down += t.download_speed || 0;
      acc.up += t.upload_speed || 0;
      return acc;
    },
    { known: 0, connected: 0, pending: 0, halfOpen: 0, seeds: 0, down: 0, up: 0 }
  );
});

const flagClass = (enabled: boolean) => enabled ? 'bg-green-100 text-green-800' : 'bg-red-100 text-red-800';

onMounted(() => {
  loadHealth();
  timer = window.setInterval(loadHealth, 5000);
});

onUnmounted(() => {
  if (timer) window.clearInterval(timer);
});
</script>

<template>
  <section class="bg-white shadow rounded-lg overflow-hidden border border-gray-200">
    <div class="px-5 py-4 border-b border-gray-200 flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between">
      <div>
        <h2 class="text-lg font-semibold text-gray-900">Engine Diagnostics</h2>
        <p class="text-sm text-gray-500">Swarm status without exposing peer addresses</p>
      </div>
      <button
        class="inline-flex items-center px-3 py-2 border border-gray-300 text-sm font-medium rounded-md text-gray-700 bg-white hover:bg-gray-50 focus:outline-none focus:ring-2 focus:ring-offset-2 focus:ring-blue-500"
        @click="loadHealth"
      >
        Refresh
      </button>
    </div>

    <div v-if="error" class="px-5 py-4 text-sm text-red-700 bg-red-50 border-b border-red-100">
      {{ error }}
    </div>

    <div v-else-if="health" class="p-5 space-y-4">
      <div class="grid grid-cols-2 gap-2 md:grid-cols-4 xl:grid-cols-8">
        <span :class="['rounded-full px-2.5 py-1 text-xs font-semibold', flagClass(health.seed_enabled)]">Seed {{ health.seed_enabled ? 'on' : 'off' }}</span>
        <span :class="['rounded-full px-2.5 py-1 text-xs font-semibold', flagClass(health.upload_enabled)]">Upload {{ health.upload_enabled ? 'on' : 'off' }}</span>
        <span :class="['rounded-full px-2.5 py-1 text-xs font-semibold', flagClass(health.dht_enabled)]">DHT {{ health.dht_enabled ? 'on' : 'off' }}</span>
        <span :class="['rounded-full px-2.5 py-1 text-xs font-semibold', flagClass(health.pex_enabled)]">PEX {{ health.pex_enabled ? 'on' : 'off' }}</span>
        <span :class="['rounded-full px-2.5 py-1 text-xs font-semibold', flagClass(health.upnp_enabled)]">UPnP {{ health.upnp_enabled ? 'on' : 'off' }}</span>
        <span :class="['rounded-full px-2.5 py-1 text-xs font-semibold', flagClass(health.tcp_enabled)]">TCP {{ health.tcp_enabled ? 'on' : 'off' }}</span>
        <span :class="['rounded-full px-2.5 py-1 text-xs font-semibold', flagClass(health.utp_enabled)]">uTP {{ health.utp_enabled ? 'on' : 'off' }}</span>
        <span :class="['rounded-full px-2.5 py-1 text-xs font-semibold', flagClass(health.ipv6_enabled)]">IPv6 {{ health.ipv6_enabled ? 'on' : 'off' }}</span>
      </div>

      <div class="rounded-md border border-amber-200 bg-amber-50 px-4 py-3 text-sm text-amber-800">
        {{ health.incoming_connectivity_note }} Listen port: {{ health.listen_port || 'auto' }}.
      </div>

      <div class="grid grid-cols-2 gap-4 text-sm md:grid-cols-4">
        <div>
          <div class="text-gray-500 text-xs uppercase tracking-wider">Profile</div>
          <div class="mt-1 font-medium text-gray-900">{{ health.client_profile }}</div>
        </div>
        <div>
          <div class="text-gray-500 text-xs uppercase tracking-wider">Retrackers</div>
          <div class="mt-1 font-medium text-gray-900">{{ health.retrackers_mode }}</div>
        </div>
        <div>
          <div class="text-gray-500 text-xs uppercase tracking-wider">Peers</div>
          <div class="mt-1 font-medium text-gray-900">{{ totals.connected }} / {{ totals.known }}</div>
        </div>
        <div>
          <div class="text-gray-500 text-xs uppercase tracking-wider">Global Speed</div>
          <div class="mt-1 font-medium text-gray-900">↓ {{ formatSpeed(totals.down) }} · ↑ {{ formatSpeed(totals.up) }}</div>
        </div>
      </div>

      <div v-if="health.torrents.length" class="overflow-x-auto">
        <table class="min-w-full divide-y divide-gray-200 text-sm">
          <thead class="bg-gray-50 text-xs uppercase tracking-wider text-gray-500">
            <tr>
              <th class="px-4 py-3 text-left font-medium">Torrent</th>
              <th class="px-4 py-3 text-left font-medium">Peers</th>
              <th class="px-4 py-3 text-left font-medium">Tracker</th>
              <th class="px-4 py-3 text-left font-medium">Speed</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-gray-200 bg-white">
            <tr v-for="torrent in health.torrents.slice(0, 6)" :key="torrent.hash">
              <td class="px-4 py-3 max-w-xs truncate font-medium text-gray-900" :title="torrent.name || torrent.hash">{{ torrent.name || torrent.hash }}</td>
              <td class="px-4 py-3 text-gray-500">
                {{ torrent.connected }} / {{ torrent.known }}
                <span class="text-gray-400">seeds {{ torrent.seeds }}</span>
              </td>
              <td class="px-4 py-3 max-w-xs truncate" :title="torrent.tracker_error || torrent.tracker_status">
                <span :class="torrent.tracker_error ? 'text-amber-700' : 'text-gray-500'">{{ torrent.tracker_error || torrent.tracker_status || 'n/a' }}</span>
              </td>
              <td class="px-4 py-3 text-gray-500">↓ {{ formatSpeed(torrent.download_speed) }} · ↑ {{ formatSpeed(torrent.upload_speed) }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <div v-else class="px-5 py-4 text-sm text-gray-500">Loading BitTorrent health...</div>
  </section>
</template>
