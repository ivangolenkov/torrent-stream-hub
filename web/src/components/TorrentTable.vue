<script setup lang="ts">
import { useTorrentStore } from '../stores/torrentStore';
import { PlayIcon, PauseIcon, TrashIcon } from '@heroicons/vue/24/solid';
import type { Torrent } from '../types';

const store = useTorrentStore();

const formatSize = (bytes: number) => {
  if (!bytes) return '0 B';
  const k = 1024;
  const sizes = ['B', 'KB', 'MB', 'GB', 'TB'];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return parseFloat((bytes / Math.pow(k, i)).toFixed(2)) + ' ' + sizes[i];
};

const formatSpeed = (bytesPerSec: number) => {
  if (!bytesPerSec) return '0 B/s';
  const k = 1024;
  const sizes = ['B/s', 'KB/s', 'MB/s', 'GB/s'];
  const i = Math.floor(Math.log(bytesPerSec) / Math.log(k));
  return parseFloat((bytesPerSec / Math.pow(k, i)).toFixed(2)) + ' ' + sizes[i];
};

const getStateColor = (state: string) => {
  const map: Record<string, string> = {
    'Streaming': 'bg-purple-100 text-purple-800',
    'Downloading': 'bg-blue-100 text-blue-800',
    'Seeding': 'bg-green-100 text-green-800',
    'Queued': 'bg-gray-100 text-gray-800',
    'Paused': 'bg-yellow-100 text-yellow-800',
    'Error': 'bg-red-100 text-red-800',
    'DiskFull': 'bg-orange-100 text-orange-800',
    'MissingFiles': 'bg-red-100 text-red-800'
  };
  return map[state] || 'bg-gray-100 text-gray-800';
};

const getPeerSummary = (torrent: Torrent) => torrent.peer_summary || {
  known: torrent.peers || 0,
  connected: torrent.peers || 0,
  pending: 0,
  half_open: 0,
  seeds: torrent.seeds || 0,
  metadata_ready: torrent.size > 0,
  dht_status: '',
  tracker_status: ''
};

const formatPeerTitle = (torrent: Torrent) => {
  const peers = getPeerSummary(torrent);
  const metadata = peers.metadata_ready ? 'ready' : 'pending';
  return `Known: ${peers.known}, connected: ${peers.connected}, pending: ${peers.pending}, half-open: ${peers.half_open}, seeds: ${peers.seeds}, metadata: ${metadata}`;
};
</script>

<template>
  <div class="overflow-x-auto">
    <table class="min-w-full divide-y divide-gray-200">
      <thead class="bg-gray-50">
        <tr>
          <th scope="col" class="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">Name</th>
          <th scope="col" class="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">Size</th>
          <th scope="col" class="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">Progress</th>
          <th scope="col" class="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">Status</th>
          <th scope="col" class="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">Speed</th>
          <th scope="col" class="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">Peers</th>
          <th scope="col" class="relative px-6 py-3"><span class="sr-only">Actions</span></th>
        </tr>
      </thead>
      <tbody class="bg-white divide-y divide-gray-200">
        <tr v-if="store.torrents.length === 0">
          <td colspan="7" class="px-6 py-12 text-center text-gray-500 text-sm">
            No torrents added yet
          </td>
        </tr>
        <tr 
          v-for="t in store.torrents" 
          :key="t.hash"
          @click="store.selectTorrent(t.hash)"
          :class="[
            'cursor-pointer transition-colors duration-150',
            store.selectedHash === t.hash ? 'bg-blue-50' : 'hover:bg-gray-50'
          ]"
        >
          <td class="px-6 py-4 whitespace-nowrap">
            <div class="flex items-center">
              <div class="text-sm font-medium text-gray-900 truncate max-w-[200px] lg:max-w-xs" :title="t.name">
                {{ t.name || t.hash }}
              </div>
            </div>
          </td>
          <td class="px-6 py-4 whitespace-nowrap text-sm text-gray-500">
            {{ formatSize(t.size) }}
          </td>
          <td class="px-6 py-4 whitespace-nowrap">
            <div class="w-full bg-gray-200 rounded-full h-2.5 flex">
              <div class="bg-blue-600 h-2.5 rounded-full" :style="{ width: `${t.progress}%` }"></div>
            </div>
            <div class="text-xs text-gray-500 mt-1">{{ t.progress.toFixed(1) }}%</div>
          </td>
          <td class="px-6 py-4 whitespace-nowrap">
            <span :class="['px-2 inline-flex text-xs leading-5 font-semibold rounded-full', getStateColor(t.state)]">
              {{ t.state }}
            </span>
            <div v-if="t.error" class="text-xs text-red-500 mt-1 truncate max-w-[150px]" :title="t.error">
              {{ t.error }}
            </div>
          </td>
          <td class="px-6 py-4 whitespace-nowrap text-sm text-gray-500">
            <div v-if="['Downloading', 'Streaming'].includes(t.state)">
              <span class="text-green-600">↓ {{ formatSpeed(t.download_speed) }}</span>
            </div>
            <div v-if="['Downloading', 'Streaming', 'Seeding'].includes(t.state) && t.upload_speed > 0">
              <span class="text-blue-600">↑ {{ formatSpeed(t.upload_speed) }}</span>
            </div>
            <div v-else-if="!['Downloading', 'Streaming'].includes(t.state)">
              -
            </div>
          </td>
          <td class="px-6 py-4 whitespace-nowrap text-sm text-gray-500" :title="formatPeerTitle(t)">
            <div class="font-medium text-gray-900">
              {{ getPeerSummary(t).connected }} / {{ getPeerSummary(t).known }}
            </div>
            <div class="text-xs text-gray-500">
              seeds {{ getPeerSummary(t).seeds }}
              <span v-if="getPeerSummary(t).pending || getPeerSummary(t).half_open">
                - pending {{ getPeerSummary(t).pending }} - half-open {{ getPeerSummary(t).half_open }}
              </span>
            </div>
            <div v-if="!getPeerSummary(t).metadata_ready" class="text-xs text-amber-600">
              metadata pending
            </div>
          </td>
          <td class="px-6 py-4 whitespace-nowrap text-right text-sm font-medium">
            <div class="flex items-center justify-end space-x-3">
              <button 
                v-if="['Paused', 'Error', 'DiskFull'].includes(t.state)"
                @click.stop="store.performAction(t.hash, 'resume')"
                class="text-green-600 hover:text-green-900 p-1 rounded-full hover:bg-green-50"
                title="Resume"
              >
                <PlayIcon class="w-5 h-5" />
              </button>
              <button 
                v-else
                @click.stop="store.performAction(t.hash, 'pause')"
                class="text-yellow-600 hover:text-yellow-900 p-1 rounded-full hover:bg-yellow-50"
                title="Pause"
              >
                <PauseIcon class="w-5 h-5" />
              </button>
              
              <button 
                @click.stop="store.performAction(t.hash, 'delete', true)"
                class="text-red-600 hover:text-red-900 p-1 rounded-full hover:bg-red-50"
                title="Delete with files"
              >
                <TrashIcon class="w-5 h-5" />
              </button>
            </div>
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
