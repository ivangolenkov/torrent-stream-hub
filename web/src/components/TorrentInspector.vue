<script setup lang="ts">
import { computed, ref } from 'vue';
import { useTorrentStore } from '../stores/torrentStore';
import { XMarkIcon, PlayCircleIcon } from '@heroicons/vue/24/solid';
import VideoPlayer from './VideoPlayer.vue';

const store = useTorrentStore();
const activeVideo = ref<{ hash: string, index: number, path: string } | null>(null);

const currentTorrent = computed(() => {
  return store.torrents.find(t => t.hash === store.selectedHash);
});

const currentFiles = computed(() => {
  if (!store.selectedHash) return [];
  return store.files[store.selectedHash] || [];
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
</script>

<template>
  <div v-if="currentTorrent" class="h-full flex flex-col max-h-[calc(100vh-8rem)]">
    <!-- Header -->
    <div class="px-4 py-3 bg-gray-50 border-b border-gray-200 flex justify-between items-center">
      <h3 class="text-sm font-medium text-gray-900 truncate pr-4" :title="currentTorrent.name">
        {{ currentTorrent.name || currentTorrent.hash }}
      </h3>
      <button @click="store.closeInspector()" class="text-gray-400 hover:text-gray-500">
        <XMarkIcon class="h-5 w-5" />
      </button>
    </div>

    <!-- Body: File Tree -->
    <div class="flex-1 overflow-y-auto p-4">
      <h4 class="text-xs font-semibold text-gray-500 uppercase tracking-wider mb-3">Files</h4>
      
      <div v-if="currentFiles.length === 0" class="text-sm text-gray-500 text-center py-4">
        Loading files...
      </div>
      
      <ul v-else class="divide-y divide-gray-200">
        <li v-for="file in currentFiles" :key="file.index" class="py-3 flex flex-col gap-2 hover:bg-gray-50 rounded px-2 transition-colors">
          <div class="flex items-start justify-between">
            <div class="flex-1 min-w-0 pr-4">
              <p class="text-sm font-medium text-gray-900 truncate" :title="file.path">
                {{ file.path.split('/').pop() }}
              </p>
              <p class="text-xs text-gray-500">
                {{ formatSize(file.downloaded) }} / {{ formatSize(file.size) }}
                ({{ ((file.downloaded / file.size) * 100).toFixed(1) || 0 }}%)
              </p>
            </div>
            
            <!-- Actions -->
            <div class="flex-shrink-0 flex gap-2">
              <button 
                v-if="file.is_media || file.path.endsWith('.mp4') || file.path.endsWith('.mkv')"
                @click="playVideo(currentTorrent.hash, file.index, file.path)"
                class="inline-flex items-center p-1 border border-transparent rounded-full shadow-sm text-white bg-blue-600 hover:bg-blue-700 focus:outline-none focus:ring-2 focus:ring-offset-2 focus:ring-blue-500"
                title="Play in browser"
              >
                <PlayCircleIcon class="h-5 w-5" aria-hidden="true" />
              </button>
            </div>
          </div>
          
          <!-- Progress Bar -->
          <div class="w-full bg-gray-200 rounded-full h-1.5 flex">
            <div class="bg-blue-500 h-1.5 rounded-full" :style="{ width: `${(file.downloaded / file.size) * 100}%` }"></div>
          </div>
        </li>
      </ul>
    </div>
  </div>

  <!-- Video Player Modal -->
  <VideoPlayer 
    v-if="activeVideo" 
    :hash="activeVideo.hash" 
    :index="activeVideo.index" 
    :title="activeVideo.path.split('/').pop() || ''"
    @close="activeVideo = null" 
  />
</template>
