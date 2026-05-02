<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue';
import { apiClient } from '../api/client';
import type { BTHealth } from '../types';

const health = ref<BTHealth | null>(null);
const error = ref('');
const actionMessage = ref('');
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

const hardRefresh = async (hash: string) => {
  try {
    await apiClient.action(hash, 'hard_refresh');
    actionMessage.value = 'Hard refresh requested';
    await loadHealth();
  } catch (e) {
    error.value = e instanceof Error ? e.message : 'Failed to hard refresh torrent';
  }
};

const recycleClient = async () => {
  try {
    await apiClient.recycleBTClient();
    actionMessage.value = 'BitTorrent client recycle requested';
    await loadHealth();
  } catch (e) {
    error.value = e instanceof Error ? e.message : 'Failed to recycle BitTorrent client';
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

const isBoosted = (until?: string) => !!until && new Date(until).getTime() > Date.now();

const statusLabel = (torrent: { degraded: boolean; boosted_until?: string }) => {
  if (isBoosted(torrent.boosted_until)) return 'Boosted';
  return torrent.degraded ? 'Degraded' : 'Healthy';
};

const statusClass = (torrent: { degraded: boolean; boosted_until?: string }) => {
  if (isBoosted(torrent.boosted_until)) return 'bg-blue-100 text-blue-800';
  return torrent.degraded ? 'bg-amber-100 text-amber-800' : 'bg-green-100 text-green-800';
};

const formatTime = (value?: string) => {
  if (!value) return 'never';
  return new Intl.DateTimeFormat(undefined, {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  }).format(new Date(value));
};

const canManualHardRefresh = (torrent: { hard_refresh_allowed: boolean; hard_refresh_blocked_reason?: string }) => {
  return torrent.hard_refresh_allowed || torrent.hard_refresh_blocked_reason === 'waiting for soft refresh attempts';
};

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
      <div class="flex flex-col gap-2 sm:flex-row sm:items-center">
        <button
          class="inline-flex items-center justify-center px-3 py-2 border border-gray-300 text-sm font-medium rounded-md text-gray-700 bg-white hover:bg-gray-50 focus:outline-none focus:ring-2 focus:ring-offset-2 focus:ring-blue-500"
          @click="loadHealth"
        >
          Reload diagnostics
        </button>
        <button
          v-if="health"
          class="inline-flex items-center justify-center px-3 py-2 border border-blue-600 text-sm font-medium rounded-md text-white bg-blue-600 hover:bg-blue-700 disabled:cursor-not-allowed disabled:opacity-50"
          :disabled="!health.client_recycle_allowed"
          :title="health.client_recycle_blocked_reason"
          @click="recycleClient"
        >
          Recycle BT client
        </button>
      </div>
    </div>

    <div v-if="error" class="px-5 py-4 text-sm text-red-700 bg-red-50 border-b border-red-100">
      {{ error }}
    </div>

    <div v-if="actionMessage" class="px-5 py-3 text-sm text-blue-700 bg-blue-50 border-b border-blue-100">
      {{ actionMessage }}
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
        <span :class="['rounded-full px-2.5 py-1 text-xs font-semibold', flagClass(health.swarm_watchdog_enabled)]">Watchdog {{ health.swarm_watchdog_enabled ? 'on' : 'off' }}</span>
        <span :class="['rounded-full px-2.5 py-1 text-xs font-semibold', flagClass(health.hard_refresh_enabled)]">Hard refresh {{ health.hard_refresh_enabled ? 'on' : 'off' }}</span>
      </div>

      <div class="rounded-md border border-amber-200 bg-amber-50 px-4 py-3 text-sm text-amber-800">
        {{ health.incoming_connectivity_note }} Listen port: {{ health.listen_port || 'auto' }}.
      </div>

      <div class="rounded-md border border-blue-200 bg-blue-50 px-4 py-3 text-sm text-blue-800">
        Without router port-forward the service relies mostly on outgoing peers. Reload diagnostics only rereads this page; it does not change torrent state. The watchdog checks every {{ health.swarm_check_interval_sec }}s, soft-refreshes every {{ health.swarm_refresh_cooldown_sec }}s, then can recycle the BT client after {{ health.client_recycle_after_soft_fails }} soft failure(s).
      </div>

      <div v-if="health.benchmark_mode" class="rounded-md border border-purple-200 bg-purple-50 px-4 py-3 text-sm text-purple-800">
        Benchmark mode is enabled. Automatic recovery mutations are suppressed so download speed can be compared fairly.
      </div>

      <div class="rounded-md border border-gray-200 bg-gray-50 px-4 py-3 text-sm text-gray-700">
        Client recycle is the primary recovery action and recreates the BitTorrent client runtime. Hard refresh is an advanced diagnostic action for one torrent. Neither action deletes downloaded files or SQLite metadata.
        <span v-if="!health.auto_hard_refresh_enabled"> Automatic hard refresh is off.</span>
        <span v-if="health.recycle_scheduled_reason"> {{ health.recycle_scheduled_reason }}.</span>
      </div>

      <div class="grid grid-cols-2 gap-4 text-sm md:grid-cols-6">
        <div>
          <div class="text-gray-500 text-xs uppercase tracking-wider">Profile</div>
          <div class="mt-1 font-medium text-gray-900">{{ health.client_profile }} · {{ health.download_profile }}</div>
          <div class="text-xs text-gray-500">conns {{ health.established_conns_per_torrent }} · half {{ health.half_open_conns_per_torrent }}/{{ health.total_half_open_conns }}</div>
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
        <div>
          <div class="text-gray-500 text-xs uppercase tracking-wider">Watchdog</div>
          <div class="mt-1 font-medium text-gray-900">{{ health.swarm_watchdog_enabled ? 'enabled' : 'disabled' }}</div>
          <div class="text-xs text-gray-500">dial {{ health.dial_rate_limit }}/s · water {{ health.peers_low_water }}/{{ health.peers_high_water }}</div>
        </div>
        <div>
          <div class="text-gray-500 text-xs uppercase tracking-wider">Client Recycle</div>
          <div class="mt-1 font-medium text-gray-900">{{ health.client_recycle_count }} total · {{ health.client_recycle_count_last_hour }}/h</div>
          <div class="text-xs text-gray-500">{{ health.client_recycle_allowed ? 'allowed' : health.client_recycle_blocked_reason }}</div>
        </div>
        <div>
          <div class="text-gray-500 text-xs uppercase tracking-wider">Trend Ratios</div>
          <div class="mt-1 font-medium text-gray-900">P {{ health.peer_drop_ratio }} · S {{ health.seed_drop_ratio }} · V {{ health.speed_drop_ratio }}</div>
        </div>
        <div>
          <div class="text-gray-500 text-xs uppercase tracking-wider">Public IP</div>
          <div class="mt-1 font-medium text-gray-900">v4 {{ health.public_ipv4_status }}</div>
          <div class="text-xs text-gray-500">v6 {{ health.public_ipv6_status }}</div>
        </div>
      </div>

      <div v-if="health.torrents.length" class="overflow-x-auto">
        <table class="min-w-full divide-y divide-gray-200 text-sm">
          <thead class="bg-gray-50 text-xs uppercase tracking-wider text-gray-500">
            <tr>
              <th class="px-4 py-3 text-left font-medium">Torrent</th>
              <th class="px-4 py-3 text-left font-medium">Peers</th>
              <th class="px-4 py-3 text-left font-medium">Swarm</th>
              <th class="px-4 py-3 text-left font-medium">Peak</th>
              <th class="px-4 py-3 text-left font-medium">Refresh</th>
              <th class="px-4 py-3 text-left font-medium">Tracker</th>
              <th class="px-4 py-3 text-left font-medium">Speed</th>
              <th class="px-4 py-3 text-left font-medium">Action</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-gray-200 bg-white">
            <tr v-for="torrent in health.torrents.slice(0, 6)" :key="torrent.hash">
              <td class="px-4 py-3 max-w-xs truncate font-medium text-gray-900" :title="torrent.name || torrent.hash">{{ torrent.name || torrent.hash }}</td>
              <td class="px-4 py-3 text-gray-500">
                {{ torrent.connected }} / {{ torrent.known }}
                <span class="text-gray-400">seeds {{ torrent.seeds }}</span>
              </td>
              <td class="px-4 py-3 text-gray-500">
                <span :class="['inline-flex rounded-full px-2 py-0.5 text-xs font-semibold', statusClass(torrent)]">
                  {{ statusLabel(torrent) }}
                </span>
                <div class="mt-1 text-xs text-gray-400">
                  refresh {{ formatTime(torrent.last_refresh_at) }}
                </div>
                <div v-if="torrent.last_refresh_reason" class="mt-1 max-w-xs truncate text-xs text-gray-500" :title="torrent.last_refresh_reason">
                  {{ torrent.last_refresh_reason }}
                </div>
                <div v-if="torrent.active_streams" class="mt-1 text-xs text-blue-600">streams {{ torrent.active_streams }}</div>
                <div v-if="torrent.degradation_episode_started_at" class="mt-1 text-xs text-gray-400">episode {{ formatTime(torrent.degradation_episode_started_at) }}</div>
              </td>
              <td class="px-4 py-3 text-gray-500">
                <div>{{ torrent.connected }} / {{ torrent.peak_connected || 0 }} peers</div>
                <div>{{ torrent.seeds }} / {{ torrent.peak_seeds || 0 }} seeds</div>
                <div>peak ↓ {{ formatSpeed(torrent.peak_download_speed || 0) }}</div>
                <div class="text-xs text-gray-400">{{ formatTime(torrent.peak_updated_at) }}</div>
              </td>
              <td class="px-4 py-3 text-gray-500">
                <div>soft {{ torrent.soft_refresh_count || 0 }} · hard {{ torrent.hard_refresh_count || 0 }}</div>
                <div class="text-xs text-gray-500">episode soft {{ torrent.soft_refresh_attempts_in_episode || 0 }} · hard {{ torrent.hard_refresh_attempts_in_episode || 0 }}</div>
                <div class="text-xs text-gray-400">metadata {{ torrent.metadata_ready ? 'ready' : 'pending' }}</div>
                <div class="text-xs text-gray-400">re-add {{ torrent.last_readd_source || 'n/a' }}</div>
                <div class="text-xs text-gray-400">hard {{ formatTime(torrent.last_hard_refresh_at) }}</div>
                <div class="text-xs text-gray-400">next {{ formatTime(torrent.next_hard_refresh_at) }}</div>
                <div v-if="torrent.last_readd_source === 'magnet'" class="mt-1 max-w-xs text-xs text-amber-700">
                  Magnet re-add may need metadata from swarm again.
                </div>
                <div v-if="torrent.last_hard_refresh_reason" class="mt-1 max-w-xs truncate text-xs text-gray-500" :title="torrent.last_hard_refresh_reason">
                  {{ torrent.last_hard_refresh_reason }}
                </div>
                <div v-if="torrent.last_hard_refresh_error" class="mt-1 max-w-xs truncate text-xs text-red-600" :title="torrent.last_hard_refresh_error">
                  {{ torrent.last_hard_refresh_error }}
                </div>
                <span v-if="!torrent.hard_refresh_allowed" class="mt-1 inline-flex rounded-full bg-gray-100 px-2 py-0.5 text-xs font-semibold text-gray-700" :title="torrent.hard_refresh_blocked_reason">
                  blocked: {{ torrent.hard_refresh_blocked_reason }}
                </span>
              </td>
              <td class="px-4 py-3 max-w-xs truncate" :title="torrent.tracker_error || torrent.tracker_status">
                <span :class="torrent.tracker_error ? 'text-amber-700' : 'text-gray-500'">{{ torrent.tracker_error || torrent.tracker_status || 'n/a' }}</span>
                <div class="text-xs text-gray-400">{{ torrent.tracker_tiers_count || 0 }} tiers · {{ torrent.tracker_urls_count || 0 }} urls</div>
              </td>
              <td class="px-4 py-3 text-gray-500">
                <div>useful ↓ {{ formatSpeed(torrent.useful_download_speed || torrent.download_speed) }}</div>
                <div>data ↓ {{ formatSpeed(torrent.data_download_speed || 0) }}</div>
                <div>raw ↓ {{ formatSpeed(torrent.raw_download_speed || 0) }}</div>
                <div>↑ {{ formatSpeed(torrent.upload_speed) }} · waste {{ ((torrent.waste_ratio || 0) * 100).toFixed(1) }}%</div>
              </td>
              <td class="px-4 py-3">
                <button
                  class="rounded-md border border-gray-300 bg-white px-2 py-1 text-xs font-medium text-gray-700 hover:bg-gray-50 disabled:cursor-not-allowed disabled:opacity-50"
                  :disabled="!canManualHardRefresh(torrent)"
                  :title="torrent.hard_refresh_blocked_reason"
                  @click="hardRefresh(torrent.hash)"
                >
                  Hard refresh (advanced)
                </button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <div v-else class="px-5 py-4 text-sm text-gray-500">Loading BitTorrent health...</div>
  </section>
</template>
