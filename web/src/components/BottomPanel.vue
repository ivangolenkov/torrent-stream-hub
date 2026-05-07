<script setup lang="ts">
import { computed, ref, onMounted, onUnmounted, watch } from 'vue';
import { useTorrentStore } from '../stores/torrentStore';
import { PlayCircleIcon, DocumentIcon } from '@heroicons/vue/24/solid';
import { ArrowDownTrayIcon, ClockIcon } from '@heroicons/vue/24/outline';
import { apiClient } from '../api/client';
import VideoPlayer from './VideoPlayer.vue';
import PieceProgressBar from './PieceProgressBar.vue';

const store = useTorrentStore();
const activeVideo = ref<{ hash: string, index: number, path: string } | null>(null);
const activeTab = ref<'general' | 'files'>('general');

const currentTorrent = computed(() => {
  return store.torrents.find(t => t.hash === store.selectedHash);
});

const currentFiles = computed(() => {
  if (!store.selectedHash) return [];
  if (currentTorrent.value && currentTorrent.value.files && currentTorrent.value.files.length > 0) {
    return currentTorrent.value.files;
  }
  return store.files[store.selectedHash] || [];
});

const peerSummary = computed(() => {
  const torrent = currentTorrent.value;
  return torrent?.peer_summary || {
    known: torrent?.peers || 0,
    connected: torrent?.peers || 0,
    pending: 0,
    half_open: 0,
    seeds: torrent?.seeds || 0,
    metadata_ready: Boolean(torrent?.size),
    dht_status: '',
    tracker_status: '',
    tracker_error: ''
  };
});

const formatSize = (bytes: number) => {
  if (!bytes) return '0 B';
  const k = 1024;
  const sizes = ['B', 'KB', 'MB', 'GB'];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return parseFloat((bytes / Math.pow(k, i)).toFixed(2)) + ' ' + sizes[i];
};

const playVideo = (hash: string, index: number, path: string) => {
  activeVideo.value = { hash, index, path };
};

const canDownloadFile = (file: { size: number; downloaded: number }) => file.size > 0 && file.downloaded >= file.size;

const downloadFile = (hash: string, index: number) => {
  window.location.href = apiClient.fileDownloadUrl(hash, index);
};

// Resizer logic
const panelHeight = ref(300);
const isDragging = ref(false);
const minHeight = 150;
const maxHeight = 800;

const startDrag = (e: MouseEvent) => {
  if (window.innerWidth < 768) return; // Disable drag on mobile
  isDragging.value = true;
  document.body.style.cursor = 'row-resize';
  e.preventDefault(); 
};

const onDrag = (e: MouseEvent) => {
  if (!isDragging.value) return;
  const newHeight = window.innerHeight - e.clientY;
  if (newHeight >= minHeight && newHeight <= maxHeight) {
    panelHeight.value = newHeight;
  }
};

const stopDrag = () => {
  isDragging.value = false;
  document.body.style.cursor = 'default';
};

const isMobile = ref(window.innerWidth < 768);

const checkMobile = () => {
  isMobile.value = window.innerWidth < 768;
};

onMounted(() => {
  window.addEventListener('resize', checkMobile);
  window.addEventListener('mousemove', onDrag);
  window.addEventListener('mouseup', stopDrag);
});

onUnmounted(() => {
  window.removeEventListener('resize', checkMobile);
  window.removeEventListener('mousemove', onDrag);
  window.removeEventListener('mouseup', stopDrag);
});

// Pieces fetching loop
const piecesString = ref('');
let piecesInterval: number | null = null;

const fetchPieces = async () => {
  if (!store.selectedHash || activeTab.value !== 'files') return;
  try {
    const res = await fetch(`/api/v1/torrent/${store.selectedHash}/pieces`);
    if (res.ok) {
      piecesString.value = await res.text();
    }
  } catch (err) {
    console.error('Failed to fetch pieces', err);
  }
};

watch([() => store.selectedHash, activeTab], ([newHash, newTab]) => {
  piecesString.value = '';
  if (piecesInterval) {
    clearInterval(piecesInterval);
    piecesInterval = null;
  }
  
  if (newHash && newTab === 'files') {
    fetchPieces();
    piecesInterval = window.setInterval(fetchPieces, 2000);
  }
});
</script>

<template>
  <transition name="slide-up">
    <div v-if="currentTorrent" 
         class="fixed bottom-0 left-0 right-0 bg-white shadow-[0_-10px_15px_-3px_rgba(0,0,0,0.1)] md:shadow-[0_-4px_6px_-1px_rgba(0,0,0,0.1)] border-t border-gray-200 flex flex-col z-40 md:transition-none"
         :style="isMobile ? {} : { height: `${panelHeight}px` }"
         :class="['h-[85vh]', 'md:h-auto']"
    >
      <!-- Resizer Handle -->
      <div 
        class="h-1.5 w-full bg-gray-200 hover:bg-blue-400 cursor-row-resize flex-shrink-0 hidden md:block"
        @mousedown="startDrag"
      ></div>
      
      <!-- Mobile Draggable Handle visual indicator -->
      <div class="w-full flex justify-center py-2 md:hidden bg-gray-50 rounded-t-xl" @click="store.closeInspector()">
        <div class="w-12 h-1.5 bg-gray-300 rounded-full"></div>
      </div>

      <!-- Header / Tabs -->
      <div class="px-4 bg-gray-50 border-b border-gray-200 flex justify-between items-end flex-shrink-0 pt-0 md:pt-2">
        <div class="flex items-center gap-4">
          <h3 class="text-sm font-bold text-gray-900 pb-2 mr-4 max-w-[150px] sm:max-w-xs truncate" :title="currentTorrent.name">
            {{ currentTorrent.name || currentTorrent.hash }}
          </h3>
          
          <button 
            @click="activeTab = 'general'"
            class="px-2 md:px-3 pb-2 text-sm font-medium border-b-2 transition-colors"
            :class="activeTab === 'general' ? 'border-blue-500 text-blue-600' : 'border-transparent text-gray-500 hover:text-gray-700 hover:border-gray-300'"
          >
            General
          </button>
          <button 
            @click="activeTab = 'files'"
            class="px-2 md:px-3 pb-2 text-sm font-medium border-b-2 transition-colors"
            :class="activeTab === 'files' ? 'border-blue-500 text-blue-600' : 'border-transparent text-gray-500 hover:text-gray-700 hover:border-gray-300'"
          >
            Files
          </button>
        </div>
        <button @click="store.closeInspector()" class="text-gray-400 hover:text-gray-600 pb-2 hidden md:block">
          <svg xmlns="http://www.w3.org/2000/svg" class="h-5 w-5" viewBox="0 0 20 20" fill="currentColor">
            <path fill-rule="evenodd" d="M4.293 4.293a1 1 0 011.414 0L10 8.586l4.293-4.293a1 1 0 111.414 1.414L11.414 10l4.293 4.293a1 1 0 01-1.414 1.414L10 11.414l-4.293 4.293a1 1 0 01-1.414-1.414L8.586 10 4.293 5.707a1 1 0 010-1.414z" clip-rule="evenodd" />
          </svg>
        </button>
      </div>

      <!-- Body Area -->
      <div class="flex-1 overflow-hidden p-4 bg-white relative h-full flex flex-col">
        
        <!-- General Tab -->
        <div v-if="activeTab === 'general'" class="max-w-4xl h-full overflow-y-auto">
          <h4 class="text-xs font-semibold text-gray-500 uppercase tracking-wider mb-3">Peer Diagnostics</h4>
          <div class="grid grid-cols-2 md:grid-cols-4 gap-4 mb-6 text-sm">
            <div class="rounded-lg border border-gray-200 bg-gray-50 p-3">
              <div class="text-xs text-gray-500 mb-1">Connected / Known</div>
              <div class="text-xl font-semibold text-gray-900">{{ peerSummary.connected }} / {{ peerSummary.known }}</div>
            </div>
            <div class="rounded-lg border border-gray-200 bg-gray-50 p-3">
              <div class="text-xs text-gray-500 mb-1">Seeds</div>
              <div class="text-xl font-semibold text-gray-900">{{ peerSummary.seeds }}</div>
            </div>
            <div class="rounded-lg border border-gray-200 bg-gray-50 p-3">
              <div class="text-xs text-gray-500 mb-1">Pending</div>
              <div class="text-xl font-semibold text-gray-900">{{ peerSummary.pending }}</div>
            </div>
            <div class="rounded-lg border border-gray-200 bg-gray-50 p-3">
              <div class="text-xs text-gray-500 mb-1">Half-open</div>
              <div class="text-xl font-semibold text-gray-900">{{ peerSummary.half_open }}</div>
            </div>
          </div>

          <div class="rounded-lg bg-gray-50 border border-gray-200 p-4 text-sm text-gray-700 space-y-2">
            <div class="flex justify-between items-center border-b border-gray-200 pb-2">
              <span class="text-gray-500">Metadata:</span>
              <span :class="peerSummary.metadata_ready ? 'text-green-600 font-medium' : 'text-amber-600 font-medium'">
                {{ peerSummary.metadata_ready ? 'Ready' : 'Pending' }}
              </span>
            </div>
            <div class="flex justify-between items-center border-b border-gray-200 pb-2">
              <span class="text-gray-500">Pieces:</span>
              <span class="font-medium" v-if="currentTorrent.num_pieces">{{ currentTorrent.num_pieces }} x {{ formatSize(currentTorrent.piece_length) }}</span>
              <span class="font-medium" v-else>Loading...</span>
            </div>
            <div class="flex justify-between items-center border-b border-gray-200 pb-2">
              <span class="text-gray-500">DHT:</span>
              <span class="font-medium">{{ peerSummary.dht_status || 'Unknown' }}</span>
            </div>
            <div class="flex justify-between items-center border-b border-gray-200 pb-2">
              <span class="text-gray-500">Tracker:</span>
              <span class="font-medium">{{ peerSummary.tracker_status || 'Unknown' }}</span>
            </div>
            <div v-if="peerSummary.tracker_error" class="flex flex-col pt-1 text-red-600">
              <span class="text-xs font-semibold uppercase mb-1">Tracker Error:</span>
              <span class="font-mono text-xs bg-red-50 p-2 rounded border border-red-100">{{ peerSummary.tracker_error }}</span>
            </div>
          </div>
        </div>

        <!-- Files Tab -->
        <div v-if="activeTab === 'files'" class="h-full flex flex-col">
          <!-- Empty State -->
          <div v-if="currentFiles.length === 0" class="flex-1 flex flex-col items-center justify-center p-6 text-center">
            <div class="bg-gray-50 rounded-full p-4 mb-4 border border-dashed border-gray-300">
              <ClockIcon v-if="!peerSummary.metadata_ready" class="h-10 w-10 text-amber-400 animate-pulse" />
              <DocumentIcon v-else class="h-10 w-10 text-gray-400" />
            </div>
            <h3 class="text-sm font-medium text-gray-900 mb-1">
              {{ peerSummary.metadata_ready ? 'Loading files...' : 'Waiting for torrent metadata...' }}
            </h3>
            <p class="text-xs text-gray-500 max-w-xs">
              {{ peerSummary.metadata_ready ? 'Parsing the file list, please wait.' : 'Connecting to peers to retrieve file information.' }}
            </p>
          </div>
          
          <div v-else class="flex-1 overflow-auto rounded-lg border border-gray-200 md:border-t-0 md:rounded-none md:border-x-0 md:border-b-0">
            <!-- Desktop Table View -->
            <table class="hidden md:table min-w-full divide-y divide-gray-200 text-sm bg-white">
              <thead class="bg-gray-50 sticky top-0 z-[5] shadow-sm">
                <tr>
                  <th scope="col" class="px-4 py-2 text-left font-semibold text-gray-500 bg-gray-50">Name</th>
                  <th scope="col" class="px-4 py-2 text-right font-semibold text-gray-500 w-32 bg-gray-50">Size</th>
                  <th scope="col" class="px-4 py-2 text-right font-semibold text-gray-500 w-24 bg-gray-50">Progress</th>
                  <th scope="col" class="px-4 py-2 text-left font-semibold text-gray-500 min-w-[200px] max-w-[400px] bg-gray-50">Pieces</th>
                  <th scope="col" class="px-4 py-2 text-center font-semibold text-gray-500 w-24 bg-gray-50">Priority</th>
                  <th scope="col" class="px-4 py-2 text-center font-semibold text-gray-500 w-24 bg-gray-50">Actions</th>
                </tr>
              </thead>
              <tbody class="divide-y divide-gray-100">
                <tr v-for="file in currentFiles" :key="file.index" class="hover:bg-gray-50">
                  <td class="px-4 py-2 whitespace-nowrap truncate max-w-[200px]" :title="file.path">
                    {{ file.path.split('/').pop() }}
                  </td>
                  <td class="px-4 py-2 whitespace-nowrap text-right text-gray-500">
                    {{ formatSize(file.size) }}
                  </td>
                  <td class="px-4 py-2 whitespace-nowrap text-right font-mono">
                    {{ ((file.downloaded / file.size) * 100).toFixed(1) }}%
                  </td>
                  <td class="px-4 py-2 w-full">
                    <PieceProgressBar 
                      :file="file" 
                      :torrent="currentTorrent" 
                      :pieces-string="piecesString" 
                    />
                  </td>
                  <td class="px-4 py-2 whitespace-nowrap text-center">
                    <select 
                      :value="file.priority" 
                      @change="store.setFilePriority(currentTorrent.hash, file.index, Number(($event.target as HTMLSelectElement).value))"
                      class="block w-auto py-0.5 pl-2.5 pr-6 text-xs font-medium rounded-full border cursor-pointer transition-colors outline-none focus:ring-2 focus:ring-offset-1 focus:ring-blue-500"
                      :class="{
                        'bg-red-50 text-red-700 border-red-200 hover:bg-red-100': file.priority === -1,
                        'bg-green-50 text-green-700 border-green-200 hover:bg-green-100': file.priority === 1,
                        'bg-gray-50 text-gray-700 border-gray-200 hover:bg-gray-100': file.priority === 0
                      }"
                    >
                      <option :value="-1">Skip</option>
                      <option :value="0">Normal</option>
                      <option :value="1">High</option>
                    </select>
                  </td>
                  <td class="px-4 py-2 whitespace-nowrap text-center">
                    <button
                      v-if="canDownloadFile(file)"
                      @click="downloadFile(currentTorrent.hash, file.index)"
                      class="inline-flex items-center justify-center text-gray-600 hover:text-gray-900 mr-2"
                      title="Download file"
                    >
                      <ArrowDownTrayIcon class="h-5 w-5" aria-hidden="true" />
                    </button>
                    <button 
                      v-if="file.is_media || file.path.endsWith('.mp4') || file.path.endsWith('.mkv')"
                      @click="playVideo(currentTorrent.hash, file.index, file.path)"
                      class="inline-flex items-center justify-center text-blue-600 hover:text-blue-800"
                      title="Play in browser"
                    >
                      <PlayCircleIcon class="h-6 w-6" aria-hidden="true" />
                    </button>
                  </td>
                </tr>
              </tbody>
            </table>

            <!-- Mobile Card View -->
            <div class="md:hidden flex flex-col divide-y divide-gray-100 pb-16">
              <div v-for="file in currentFiles" :key="file.index" class="p-4 bg-white hover:bg-gray-50 flex flex-col gap-2">
                <!-- Top row: Name and Actions -->
                <div class="flex justify-between items-start gap-2">
                  <div class="text-sm font-medium text-gray-900 break-all" :title="file.path">
                    {{ file.path.split('/').pop() }}
                  </div>
                  <div class="flex items-center gap-2 flex-shrink-0">
                    <button
                      v-if="canDownloadFile(file)"
                      @click="downloadFile(currentTorrent.hash, file.index)"
                      class="p-1.5 text-gray-500 hover:text-gray-900 bg-gray-100 rounded-md"
                      title="Download file"
                    >
                      <ArrowDownTrayIcon class="h-4 w-4" aria-hidden="true" />
                    </button>
                    <button 
                      v-if="file.is_media || file.path.endsWith('.mp4') || file.path.endsWith('.mkv')"
                      @click="playVideo(currentTorrent.hash, file.index, file.path)"
                      class="p-1.5 text-blue-600 hover:text-blue-800 bg-blue-50 rounded-md"
                      title="Play in browser"
                    >
                      <PlayCircleIcon class="h-4 w-4" aria-hidden="true" />
                    </button>
                  </div>
                </div>
                
                <!-- Info row: Size and Progress -->
                <div class="flex justify-between items-center text-xs text-gray-500">
                  <div class="flex items-center gap-2">
                    <span>{{ formatSize(file.size) }}</span>
                    <select 
                      :value="file.priority" 
                      @change="store.setFilePriority(currentTorrent.hash, file.index, Number(($event.target as HTMLSelectElement).value))"
                      class="block w-auto py-0.5 pl-2 pr-5 text-xs font-medium rounded-full border cursor-pointer transition-colors outline-none focus:ring-2 focus:ring-offset-1 focus:ring-blue-500"
                      :class="{
                        'bg-red-50 text-red-700 border-red-200 hover:bg-red-100': file.priority === -1,
                        'bg-green-50 text-green-700 border-green-200 hover:bg-green-100': file.priority === 1,
                        'bg-gray-50 text-gray-700 border-gray-200 hover:bg-gray-100': file.priority === 0
                      }"
                    >
                      <option :value="-1">Skip</option>
                      <option :value="0">Normal</option>
                      <option :value="1">High</option>
                    </select>
                  </div>
                  <span class="font-mono font-medium text-gray-700">{{ ((file.downloaded / file.size) * 100).toFixed(1) }}%</span>
                </div>

                <!-- Piece Progress Bar -->
                <div class="w-full mt-1">
                  <PieceProgressBar 
                    :file="file" 
                    :torrent="currentTorrent" 
                    :pieces-string="piecesString" 
                  />
                </div>
              </div>
            </div>

          </div>
        </div>
      </div>
    </div>
  </transition>

  <!-- Video Player Modal -->
  <VideoPlayer 
    v-if="activeVideo" 
    :hash="activeVideo.hash" 
    :index="activeVideo.index" 
    :title="activeVideo.path.split('/').pop() || ''"
    @close="activeVideo = null" 
  />
</template>

<style scoped>
.slide-up-enter-active,
.slide-up-leave-active {
  transition: transform 0.3s ease-out;
}
.slide-up-enter-from,
.slide-up-leave-to {
  transform: translateY(100%);
}
</style>
