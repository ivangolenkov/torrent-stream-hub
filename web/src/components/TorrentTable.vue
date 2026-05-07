<script setup lang="ts">
import { computed, ref, onMounted, onUnmounted, nextTick } from 'vue';
import { useTorrentStore } from '../stores/torrentStore';
import { PlayIcon, PauseIcon, TrashIcon, EllipsisVerticalIcon, FolderOpenIcon, PlusIcon } from '@heroicons/vue/24/solid';
import { ArrowDownTrayIcon, FolderMinusIcon } from '@heroicons/vue/24/outline';
import { apiClient } from '../api/client';
import type { Torrent } from '../types';
import ConfirmDialog from './ConfirmDialog.vue';

const store = useTorrentStore();
const emit = defineEmits(['add-torrent']);
const pendingDelete = ref<{ torrent: Torrent; deleteFiles: boolean } | null>(null);
const deleting = ref(false);
const activeMenu = ref<string | null>(null);
const activeTorrent = computed(() => store.torrents.find(t => t.hash === activeMenu.value));
const menuRef = ref<HTMLElement | null>(null);
const buttonRef = ref<HTMLElement | null>(null);
const menuStyle = ref({ top: '0px', left: '0px' });

const handleClickOutside = (event: MouseEvent) => {
  if (activeMenu.value && menuRef.value && buttonRef.value) {
    const isClickInsideMenu = menuRef.value.contains(event.target as Node);
    const isClickInsideButton = buttonRef.value.contains(event.target as Node);
    if (!isClickInsideMenu && !isClickInsideButton) {
      activeMenu.value = null;
    }
  }
};

const toggleMenu = async (hash: string, event: MouseEvent) => {
  event.stopPropagation();
  if (activeMenu.value === hash) {
    activeMenu.value = null;
  } else {
    activeMenu.value = hash;
    await nextTick();
    const button = event.currentTarget as HTMLElement;
    buttonRef.value = button;
    const rect = button.getBoundingClientRect();
    
    const spaceBelow = window.innerHeight - rect.bottom;
    const menuHeight = 200;
    
    if (spaceBelow < menuHeight && rect.top > menuHeight) {
      menuStyle.value = {
        top: `${rect.top - menuHeight}px`,
        left: `${rect.right - 224}px`
      };
    } else {
      menuStyle.value = {
        top: `${rect.bottom}px`,
        left: `${rect.right - 224}px`
      };
    }
  }
};

onMounted(() => {
  document.addEventListener('click', handleClickOutside);
});

onUnmounted(() => {
  document.removeEventListener('click', handleClickOutside);
});

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
    'Streaming': 'bg-purple-100 text-purple-800 border-purple-200',
    'Downloading': 'bg-blue-100 text-blue-800 border-blue-200',
    'Seeding': 'bg-green-100 text-green-800 border-green-200',
    'Queued': 'bg-gray-100 text-gray-800 border-gray-200',
    'Paused': 'bg-yellow-100 text-yellow-800 border-yellow-200',
    'Error': 'bg-red-100 text-red-800 border-red-200',
    'DiskFull': 'bg-orange-100 text-orange-800 border-orange-200',
    'MissingFiles': 'bg-red-100 text-red-800 border-red-200'
  };
  return map[state] || 'bg-gray-100 text-gray-800 border-gray-200';
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

const torrentLabel = (torrent: Torrent) => torrent.name || torrent.title || torrent.hash;
const canDownloadTorrent = (torrent: Torrent) => torrent.size > 0 && torrent.downloaded >= torrent.size;

const downloadTorrent = (torrent: Torrent) => {
  window.location.href = apiClient.torrentDownloadUrl(torrent.hash);
};

const deleteTitle = computed(() => {
  if (!pendingDelete.value) return '';
  return pendingDelete.value.deleteFiles ? 'Delete Torrent And Files' : 'Remove Torrent';
});

const deleteMessage = computed(() => {
  if (!pendingDelete.value) return '';
  const name = torrentLabel(pendingDelete.value.torrent);
  return pendingDelete.value.deleteFiles
    ? `Remove "${name}" from the client and delete all downloaded files from disk.\n\nThis cannot be undone.`
    : `Remove "${name}" from the client.\n\nDownloaded files will stay on disk.`;
});

const openDeleteDialog = (torrent: Torrent, deleteFiles: boolean) => {
  pendingDelete.value = { torrent, deleteFiles };
};

const cancelDelete = () => {
  if (deleting.value) return;
  pendingDelete.value = null;
};

const confirmDelete = async () => {
  if (!pendingDelete.value) return;
  deleting.value = true;
  try {
    await store.performAction(pendingDelete.value.torrent.hash, 'delete', pendingDelete.value.deleteFiles);
    pendingDelete.value = null;
  } finally {
    deleting.value = false;
  }
};
</script>

<template>
  <div class="min-h-[300px] w-full">
    <!-- Empty State -->
    <div v-if="store.torrents.length === 0" class="flex flex-col items-center justify-center py-20 px-4">
      <div class="bg-gray-50 rounded-full p-4 mb-4 border border-dashed border-gray-300">
        <FolderOpenIcon class="h-12 w-12 text-gray-400" />
      </div>
      <h3 class="text-lg font-medium text-gray-900 mb-1">No torrents added yet</h3>
      <p class="text-sm text-gray-500 mb-6 text-center max-w-sm">
        Get started by adding a new torrent file or magnet link. Your downloads and active streams will appear here.
      </p>
      <button 
        @click="emit('add-torrent')"
        class="inline-flex items-center px-4 py-2 border border-transparent text-sm font-medium rounded-md shadow-sm text-white bg-blue-600 hover:bg-blue-700 focus:outline-none focus:ring-2 focus:ring-offset-2 focus:ring-blue-500 transition-colors"
      >
        <PlusIcon class="-ml-1 mr-2 h-5 w-5" />
        Add Torrent
      </button>
    </div>

    <!-- Desktop Table View -->
    <div v-else class="hidden md:block overflow-x-auto">
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
              <div class="flex items-center gap-3">
                <img
                  v-if="t.poster"
                  :src="t.poster"
                  alt=""
                  class="h-12 w-9 flex-none rounded object-cover bg-gray-100"
                >
                <div class="text-sm font-medium text-gray-900 truncate max-w-[200px] lg:max-w-xs" :title="torrentLabel(t)">
                  {{ torrentLabel(t) }}
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
              <span :class="['px-2.5 py-0.5 inline-flex text-xs leading-5 font-semibold rounded-full border', getStateColor(t.state)]">
                {{ t.state }}
              </span>
              <div v-if="t.error" class="text-xs text-red-500 mt-1 truncate max-w-[150px]" :title="t.error">
                {{ t.error }}
              </div>
            </td>
            <td class="px-6 py-4 whitespace-nowrap text-sm text-gray-500">
              <div v-if="['Downloading', 'Streaming'].includes(t.state)">
                <span class="text-green-600 font-medium">↓ {{ formatSpeed(t.download_speed) }}</span>
              </div>
              <div v-if="['Downloading', 'Streaming', 'Seeding'].includes(t.state) && t.upload_speed > 0">
                <span class="text-blue-600 font-medium">↑ {{ formatSpeed(t.upload_speed) }}</span>
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
              </div>
              <div v-if="!getPeerSummary(t).metadata_ready" class="text-xs text-amber-600">
                metadata pending
              </div>
            </td>
            <td class="px-6 py-4 whitespace-nowrap text-right text-sm font-medium relative">
              <button 
                @click="toggleMenu(t.hash, $event)"
                class="text-gray-400 hover:text-gray-600 p-1 rounded-full hover:bg-gray-100"
              >
                <EllipsisVerticalIcon class="w-5 h-5" />
              </button>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <!-- Mobile Card View -->
    <div v-if="store.torrents.length > 0" class="md:hidden flex flex-col divide-y divide-gray-200">
      <div 
        v-for="t in store.torrents" 
        :key="t.hash"
        @click="store.selectTorrent(t.hash)"
        :class="[
          'p-4 cursor-pointer transition-colors duration-150 relative flex gap-4',
          store.selectedHash === t.hash ? 'bg-blue-50' : 'hover:bg-gray-50 bg-white'
        ]"
      >
        <img
          v-if="t.poster"
          :src="t.poster"
          alt=""
          class="h-20 w-14 flex-none rounded object-cover bg-gray-100"
        >
        <div class="flex-1 min-w-0">
          <!-- Top row: Name + Actions -->
          <div class="flex justify-between items-start mb-1">
            <div class="text-sm font-medium text-gray-900 line-clamp-2 pr-2" :title="torrentLabel(t)">
              {{ torrentLabel(t) }}
            </div>
            <button 
              @click="toggleMenu(t.hash, $event)"
              class="text-gray-400 hover:text-gray-600 p-1 -mt-1 -mr-1 rounded-full hover:bg-gray-200 flex-shrink-0"
            >
              <EllipsisVerticalIcon class="w-5 h-5" />
            </button>
          </div>
          
          <!-- Status tag + Size -->
          <div class="flex items-center gap-2 mb-2 text-xs">
            <span :class="['px-2 py-0.5 inline-flex leading-4 font-semibold rounded border', getStateColor(t.state)]">
              {{ t.state }}
            </span>
            <span class="text-gray-500">{{ formatSize(t.size) }}</span>
          </div>

          <!-- Progress bar -->
          <div class="w-full bg-gray-200 rounded-full h-1.5 mb-1.5 flex">
            <div class="bg-blue-600 h-1.5 rounded-full" :style="{ width: `${t.progress}%` }"></div>
          </div>
          
          <!-- Bottom row: Speeds + Peers + Progress text -->
          <div class="flex justify-between items-center text-xs text-gray-500">
            <div class="flex items-center gap-3">
              <span class="font-medium text-gray-700">{{ t.progress.toFixed(1) }}%</span>
              <div v-if="['Downloading', 'Streaming'].includes(t.state)" class="flex gap-2">
                <span class="text-green-600 font-medium">↓ {{ formatSpeed(t.download_speed) }}</span>
                <span v-if="t.upload_speed > 0" class="text-blue-600 font-medium">↑ {{ formatSpeed(t.upload_speed) }}</span>
              </div>
              <div v-else-if="t.state === 'Seeding' && t.upload_speed > 0">
                <span class="text-blue-600 font-medium">↑ {{ formatSpeed(t.upload_speed) }}</span>
              </div>
            </div>
            <div class="flex items-center gap-1" :title="formatPeerTitle(t)">
              <span class="font-medium text-gray-700">{{ getPeerSummary(t).connected }}/{{ getPeerSummary(t).known }}</span> p
            </div>
          </div>
          
          <div v-if="t.error" class="text-xs text-red-500 mt-1 truncate" :title="t.error">
            {{ t.error }}
          </div>
        </div>
      </div>
    </div>

    <!-- Action Menu Dropdown -->
    <Teleport to="body">
      <div 
        v-if="activeMenu && activeTorrent" 
        ref="menuRef" 
        :style="menuStyle"
        class="fixed z-[100] w-56 rounded-md shadow-lg bg-white ring-1 ring-black ring-opacity-5"
      >
        <div class="py-1" role="menu">
          <button 
            v-if="['Paused', 'Error', 'DiskFull'].includes(activeTorrent.state)"
            @click="store.performAction(activeTorrent.hash, 'resume'); activeMenu = null"
            class="flex items-center w-full px-4 py-2 text-sm text-gray-700 hover:bg-gray-100"
          >
            <PlayIcon class="w-4 h-4 mr-2" /> Resume
          </button>
          <button 
            v-else
            @click="store.performAction(activeTorrent.hash, 'pause'); activeMenu = null"
            class="flex items-center w-full px-4 py-2 text-sm text-gray-700 hover:bg-gray-100"
          >
            <PauseIcon class="w-4 h-4 mr-2" /> Pause
          </button>
          <button 
            v-if="canDownloadTorrent(activeTorrent)"
            @click="downloadTorrent(activeTorrent); activeMenu = null"
            class="flex items-center w-full px-4 py-2 text-sm text-gray-700 hover:bg-gray-100"
          >
            <ArrowDownTrayIcon class="w-4 h-4 mr-2" /> Download
          </button>
          <button 
            @click="openDeleteDialog(activeTorrent, false); activeMenu = null"
            class="flex items-center w-full px-4 py-2 text-sm text-red-600 hover:bg-red-50"
          >
            <TrashIcon class="w-4 h-4 mr-2" /> Remove
          </button>
          <button 
            @click="openDeleteDialog(activeTorrent, true); activeMenu = null"
            class="flex items-center w-full px-4 py-2 text-sm text-red-600 hover:bg-red-50"
          >
            <FolderMinusIcon class="w-4 h-4 mr-2" /> Remove with files
          </button>
        </div>
      </div>
    </Teleport>

    <ConfirmDialog
      :is-open="Boolean(pendingDelete)"
      :title="deleteTitle"
      :message="deleteMessage"
      :confirm-label="pendingDelete?.deleteFiles ? 'Delete Files' : 'Remove Torrent'"
      :danger="true"
      :loading="deleting"
      @confirm="confirmDelete"
      @cancel="cancelDelete"
    />
  </div>
</template>
