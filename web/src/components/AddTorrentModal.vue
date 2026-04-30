<script setup lang="ts">
import { ref } from 'vue';
import { apiClient } from '../api/client';
import { XMarkIcon, DocumentArrowUpIcon } from '@heroicons/vue/24/outline';

const props = defineProps<{
  isOpen: boolean;
}>();

const emit = defineEmits(['close']);

const link = ref('');
const isDragging = ref(false);
const error = ref('');
const loading = ref(false);

const handleClose = () => {
  if (loading.value) return;
  link.value = '';
  error.value = '';
  emit('close');
};

const handleAdd = async () => {
  if (!link.value) {
    error.value = 'Please provide a magnet link or info hash';
    return;
  }
  
  loading.value = true;
  error.value = '';
  try {
    await apiClient.addTorrent(link.value, '', false);
    handleClose();
  } catch (e: any) {
    error.value = e.message || 'Failed to add torrent';
  } finally {
    loading.value = false;
  }
};

const onDragOver = (e: DragEvent) => {
  e.preventDefault();
  isDragging.value = true;
};

const onDragLeave = (e: DragEvent) => {
  e.preventDefault();
  isDragging.value = false;
};

const onDrop = async (e: DragEvent) => {
  e.preventDefault();
  isDragging.value = false;
  error.value = 'File upload not fully supported in this demo. Paste a magnet link instead.';
  
  // Real implementation for files:
  /*
  const files = e.dataTransfer?.files;
  if (files && files.length > 0) {
    const file = files[0];
    if (file.name.endsWith('.torrent')) {
      // Create FormData and send to /torrent/upload (to be implemented on backend)
    }
  }
  */
};
</script>

<template>
  <div v-if="isOpen" class="fixed z-50 inset-0 overflow-y-auto" aria-labelledby="modal-title" role="dialog" aria-modal="true">
    <div class="flex items-end justify-center min-h-screen pt-4 px-4 pb-20 text-center sm:block sm:p-0">
      
      <!-- Background overlay -->
      <div class="fixed inset-0 bg-gray-500 bg-opacity-75 transition-opacity" @click="handleClose" aria-hidden="true"></div>

      <!-- Center trick -->
      <span class="hidden sm:inline-block sm:align-middle sm:h-screen" aria-hidden="true">&#8203;</span>

      <!-- Modal panel -->
      <div class="inline-block align-bottom bg-white rounded-lg px-4 pt-5 pb-4 text-left overflow-hidden shadow-xl transform transition-all sm:my-8 sm:align-middle sm:max-w-lg sm:w-full sm:p-6">
        <div class="absolute top-0 right-0 pt-4 pr-4">
          <button @click="handleClose" type="button" class="bg-white rounded-md text-gray-400 hover:text-gray-500 focus:outline-none">
            <span class="sr-only">Close</span>
            <XMarkIcon class="h-6 w-6" />
          </button>
        </div>
        
        <div class="sm:flex sm:items-start">
          <div class="mt-3 text-center sm:mt-0 sm:ml-4 sm:text-left w-full">
            <h3 class="text-lg leading-6 font-medium text-gray-900" id="modal-title">
              Add New Torrent
            </h3>
            <div class="mt-4">
              <!-- Drag and Drop Area -->
              <div 
                @dragover="onDragOver"
                @dragleave="onDragLeave"
                @drop="onDrop"
                :class="[
                  'mt-1 flex justify-center px-6 pt-5 pb-6 border-2 border-dashed rounded-md transition-colors',
                  isDragging ? 'border-blue-500 bg-blue-50' : 'border-gray-300'
                ]"
              >
                <div class="space-y-1 text-center">
                  <DocumentArrowUpIcon class="mx-auto h-12 w-12 text-gray-400" />
                  <div class="flex text-sm text-gray-600 justify-center">
                    <label for="file-upload" class="relative cursor-pointer bg-white rounded-md font-medium text-blue-600 hover:text-blue-500 focus-within:outline-none focus-within:ring-2 focus-within:ring-offset-2 focus-within:ring-blue-500">
                      <span>Upload a file</span>
                      <input id="file-upload" name="file-upload" type="file" class="sr-only" accept=".torrent">
                    </label>
                    <p class="pl-1">or drag and drop</p>
                  </div>
                  <p class="text-xs text-gray-500">
                    .torrent files up to 10MB (coming soon)
                  </p>
                </div>
              </div>

              <!-- Magnet Link Input -->
              <div class="mt-4">
                <label for="magnet" class="block text-sm font-medium text-gray-700">Magnet Link or Info Hash</label>
                <div class="mt-1">
                  <textarea
                    id="magnet"
                    v-model="link"
                    rows="3"
                    class="shadow-sm focus:ring-blue-500 focus:border-blue-500 block w-full sm:text-sm border border-gray-300 rounded-md p-2"
                    placeholder="magnet:?xt=urn:btih:..."
                  ></textarea>
                </div>
                <p v-if="error" class="mt-2 text-sm text-red-600">{{ error }}</p>
              </div>
            </div>
          </div>
        </div>
        
        <div class="mt-5 sm:mt-4 sm:flex sm:flex-row-reverse">
          <button 
            type="button" 
            @click="handleAdd"
            :disabled="loading"
            class="w-full inline-flex justify-center rounded-md border border-transparent shadow-sm px-4 py-2 bg-blue-600 text-base font-medium text-white hover:bg-blue-700 focus:outline-none focus:ring-2 focus:ring-offset-2 focus:ring-blue-500 sm:ml-3 sm:w-auto sm:text-sm disabled:opacity-50"
          >
            {{ loading ? 'Adding...' : 'Add Torrent' }}
          </button>
          <button 
            type="button" 
            @click="handleClose"
            :disabled="loading"
            class="mt-3 w-full inline-flex justify-center rounded-md border border-gray-300 shadow-sm px-4 py-2 bg-white text-base font-medium text-gray-700 hover:bg-gray-50 focus:outline-none focus:ring-2 focus:ring-offset-2 focus:ring-blue-500 sm:mt-0 sm:w-auto sm:text-sm disabled:opacity-50"
          >
            Cancel
          </button>
        </div>
      </div>
    </div>
  </div>
</template>
