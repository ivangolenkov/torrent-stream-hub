<script setup lang="ts">
import { computed, ref } from 'vue';
import { apiClient } from '../api/client';
import { useTorrentStore } from '../stores/torrentStore';
import { DocumentArrowUpIcon, LinkIcon, XMarkIcon } from '@heroicons/vue/24/outline';

defineProps<{
  isOpen: boolean;
}>();

const emit = defineEmits(['close']);

type AddMode = 'magnet' | 'file';

const store = useTorrentStore();
const mode = ref<AddMode>('magnet');
const link = ref('');
const poster = ref('');
const selectedFile = ref<File | null>(null);
const fileInput = ref<HTMLInputElement | null>(null);
const isDragging = ref(false);
const error = ref('');
const loading = ref(false);

const canSubmit = computed(() => {
  if (loading.value) return false;
  if (mode.value === 'magnet') return link.value.trim().length > 0;
  return selectedFile.value !== null;
});

const actionLabel = computed(() => {
  if (loading.value) return mode.value === 'magnet' ? 'Adding magnet...' : 'Uploading file...';
  return mode.value === 'magnet' ? 'Add magnet' : 'Upload .torrent';
});

const resetForm = () => {
  mode.value = 'magnet';
  link.value = '';
  poster.value = '';
  selectedFile.value = null;
  error.value = '';
  isDragging.value = false;

  if (fileInput.value) {
    fileInput.value.value = '';
  }
};

const handleClose = () => {
  if (loading.value) return;
  resetForm();
  emit('close');
};

const setMode = (nextMode: AddMode) => {
  mode.value = nextMode;
  error.value = '';

  if (nextMode === 'magnet') {
    selectedFile.value = null;
  } else {
    link.value = '';
  }
};

const handleAdd = async () => {
  if (!canSubmit.value) {
    error.value = mode.value === 'magnet'
      ? 'Paste a magnet link or info hash first'
      : 'Choose a .torrent file first';
    return;
  }

  loading.value = true;
  error.value = '';

  try {
    if (mode.value === 'file' && selectedFile.value) {
      await apiClient.uploadTorrent(selectedFile.value);
    } else {
      await apiClient.addTorrent(link.value, '', false, poster.value.trim());
    }

    await store.loadTorrents();
    resetForm();
    emit('close');
  } catch (e: any) {
    error.value = e.message || 'Failed to add torrent';
  } finally {
    loading.value = false;
  }
};

const setTorrentFile = (file: File) => {
  if (!file.name.toLowerCase().endsWith('.torrent')) {
    error.value = 'Only .torrent files are supported';
    return;
  }

  mode.value = 'file';
  selectedFile.value = file;
  link.value = '';
  error.value = '';
};

const chooseFile = () => {
  fileInput.value?.click();
};

const onFileChange = (e: Event) => {
  const input = e.target as HTMLInputElement;
  const file = input.files?.[0];
  if (file) {
    setTorrentFile(file);
  }
};

const onDragOver = (e: DragEvent) => {
  e.preventDefault();
  mode.value = 'file';
  isDragging.value = true;
};

const onDragLeave = (e: DragEvent) => {
  e.preventDefault();
  isDragging.value = false;
};

const onDrop = (e: DragEvent) => {
  e.preventDefault();
  isDragging.value = false;

  const file = e.dataTransfer?.files?.[0];
  if (file) {
    setTorrentFile(file);
  }
};
</script>

<template>
  <Teleport to="body">
    <div v-if="isOpen" class="fixed inset-0" style="z-index: 1000" aria-labelledby="modal-title" role="dialog" aria-modal="true">
      <div class="fixed inset-0 bg-gray-500/75 transition-opacity" aria-hidden="true" @click="handleClose"></div>

      <div class="pointer-events-none fixed inset-0 overflow-y-auto" style="z-index: 1001">
        <div class="flex min-h-full items-end justify-center px-4 pb-20 pt-4 text-center sm:items-center sm:p-0">
          <div
            class="pointer-events-auto relative inline-block w-full max-w-xl transform overflow-hidden rounded-xl bg-white px-4 pb-4 pt-5 text-left align-bottom shadow-xl transition-all sm:my-8 sm:p-6 sm:align-middle"
            @click.stop
          >
            <div class="absolute right-0 top-0 pr-4 pt-4">
              <button type="button" class="rounded-md bg-white text-gray-400 hover:text-gray-500 focus:outline-none" @click="handleClose">
                <span class="sr-only">Close</span>
                <XMarkIcon class="h-6 w-6" />
              </button>
            </div>

            <div>
              <h3 id="modal-title" class="text-lg font-semibold leading-6 text-gray-900">
                Add torrent
              </h3>
              <p class="mt-1 text-sm text-gray-500">
                Add a magnet link instantly or upload a local .torrent file.
              </p>
            </div>

            <div class="mt-5 grid grid-cols-2 rounded-lg bg-gray-100 p-1">
              <button
                type="button"
                :class="[
                  'flex items-center justify-center gap-2 rounded-md px-3 py-2 text-sm font-medium transition',
                  mode === 'magnet' ? 'bg-white text-blue-700 shadow-sm' : 'text-gray-600 hover:text-gray-900'
                ]"
                @click="setMode('magnet')"
              >
                <LinkIcon class="h-4 w-4" />
                Magnet / hash
              </button>
              <button
                type="button"
                :class="[
                  'flex items-center justify-center gap-2 rounded-md px-3 py-2 text-sm font-medium transition',
                  mode === 'file' ? 'bg-white text-blue-700 shadow-sm' : 'text-gray-600 hover:text-gray-900'
                ]"
                @click="setMode('file')"
              >
                <DocumentArrowUpIcon class="h-4 w-4" />
                .torrent file
              </button>
            </div>

            <div class="mt-5">
              <div v-if="mode === 'magnet'">
                <label for="magnet" class="block text-sm font-medium text-gray-700">Magnet link or info hash</label>
                <textarea
                  id="magnet"
                  v-model="link"
                  rows="5"
                  class="mt-2 block w-full rounded-md border border-gray-300 p-3 text-sm shadow-sm focus:border-blue-500 focus:ring-blue-500"
                  placeholder="magnet:?xt=urn:btih:... or 40-character info hash"
                ></textarea>
                <p class="mt-2 text-xs text-gray-500">
                  Raw BTIH hashes are automatically converted to magnet links before sending.
                </p>

                <label for="poster" class="mt-4 block text-sm font-medium text-gray-700">Poster URL (optional)</label>
                <input
                  id="poster"
                  v-model="poster"
                  type="url"
                  class="mt-2 block w-full rounded-md border border-gray-300 p-3 text-sm shadow-sm focus:border-blue-500 focus:ring-blue-500"
                  placeholder="https://example.com/poster.jpg"
                >
              </div>

              <div v-else>
                <input ref="fileInput" type="file" class="hidden" accept=".torrent" @change="onFileChange">
                <div
                  :class="[
                    'flex min-h-48 flex-col items-center justify-center rounded-lg border-2 border-dashed px-6 py-8 text-center transition',
                    isDragging ? 'border-blue-500 bg-blue-50' : 'border-gray-300 bg-gray-50'
                  ]"
                  @dragover="onDragOver"
                  @dragleave="onDragLeave"
                  @drop="onDrop"
                >
                  <DocumentArrowUpIcon class="h-12 w-12 text-gray-400" />
                  <p class="mt-3 text-sm font-medium text-gray-900">
                    Drop a .torrent file here
                  </p>
                  <p class="mt-1 text-xs text-gray-500">or select it from disk</p>
                  <button
                    type="button"
                    class="mt-4 inline-flex items-center rounded-md border border-gray-300 bg-white px-4 py-2 text-sm font-medium text-gray-700 shadow-sm hover:bg-gray-50"
                    @click="chooseFile"
                  >
                    Choose file
                  </button>
                  <p v-if="selectedFile" class="mt-4 max-w-full truncate rounded-full bg-blue-100 px-3 py-1 text-xs font-medium text-blue-800">
                    {{ selectedFile.name }}
                  </p>
                </div>
              </div>

              <p v-if="error" class="mt-3 rounded-md bg-red-50 px-3 py-2 text-sm text-red-700">
                {{ error }}
              </p>
            </div>

            <div class="mt-6 flex flex-col-reverse gap-3 sm:flex-row sm:justify-end">
              <button
                type="button"
                :disabled="loading"
                class="inline-flex justify-center rounded-md border border-gray-300 bg-white px-4 py-2 text-sm font-medium text-gray-700 shadow-sm hover:bg-gray-50 disabled:opacity-50"
                @click="handleClose"
              >
                Cancel
              </button>
              <button
                type="button"
                :disabled="!canSubmit"
                class="inline-flex justify-center rounded-md border border-transparent bg-blue-600 px-4 py-2 text-sm font-medium text-white shadow-sm hover:bg-blue-700 disabled:cursor-not-allowed disabled:opacity-50"
                @click="handleAdd"
              >
                {{ actionLabel }}
              </button>
            </div>
          </div>
        </div>
      </div>
    </div>
  </Teleport>
</template>
