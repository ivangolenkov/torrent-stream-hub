<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue';
import { XMarkIcon } from '@heroicons/vue/24/solid';

const props = defineProps<{
  hash: string;
  index: number;
  title: string;
}>();

const emit = defineEmits(['close']);

const videoUrl = ref('');
const playbackError = ref(false);

const fileExtension = computed(() => {
  const cleanTitle = props.title.split('?')[0].toLowerCase();
  return cleanTitle.includes('.') ? cleanTitle.split('.').pop() || '' : '';
});

const browserPlayableHint = computed(() => {
  const playable = ['mp4', 'm4v', 'webm', 'ogv', 'ogg'];
  return playable.includes(fileExtension.value);
});

onMounted(() => {
  videoUrl.value = `/stream?hash=${props.hash}&index=${props.index}`;
  document.body.style.overflow = 'hidden';
  window.addEventListener('keydown', handleKeydown);
});

onUnmounted(() => {
  document.body.style.overflow = '';
  window.removeEventListener('keydown', handleKeydown);
});

const handleClose = () => {
  emit('close');
};

const handlePlaybackError = () => {
  playbackError.value = true;
};

const handleKeydown = (event: KeyboardEvent) => {
  if (event.key === 'Escape') {
    handleClose();
  }
};
</script>

<template>
  <Teleport to="body">
    <div class="fixed inset-0 flex flex-col overflow-hidden bg-black" style="z-index: 9000">
      <!-- Top bar -->
      <div class="pointer-events-none fixed inset-x-0 top-0 flex h-20 items-start justify-between bg-gradient-to-b from-black/95 to-transparent px-4 pt-4 sm:h-24 sm:px-6" style="z-index: 9010">
        <h2 class="pointer-events-auto max-w-[calc(100%-5rem)] truncate rounded-full bg-black/45 px-4 py-2 text-base font-medium text-white shadow-lg ring-1 ring-white/10 sm:text-lg" :title="title">
          {{ title }}
        </h2>
        <button 
          type="button"
          @click="handleClose" 
          class="pointer-events-auto flex h-12 w-12 items-center justify-center rounded-full bg-red-600 text-white shadow-2xl ring-2 ring-white/70 transition hover:bg-red-500 focus:outline-none focus:ring-4 focus:ring-white"
          style="z-index: 9020"
          aria-label="Close player"
          title="Close player"
        >
          <XMarkIcon class="h-8 w-8" />
        </button>
      </div>

      <!-- Video Container -->
      <div class="flex h-full w-full flex-1 items-center justify-center bg-black pt-16">
        <video 
          ref="videoPlayer"
          controls 
          autoplay 
          class="h-full max-h-[calc(100vh-4rem)] w-full object-contain"
          :src="videoUrl"
          @error="handlePlaybackError"
        >
          Your browser does not support the video tag.
        </video>

        <div
          v-if="playbackError || !browserPlayableHint"
          class="absolute bottom-6 left-1/2 w-[calc(100%-2rem)] max-w-2xl -translate-x-1/2 rounded-xl border border-white/10 bg-black/85 p-4 text-sm text-white shadow-2xl"
          style="z-index: 9010"
        >
          <div class="font-semibold">Browser playback may not be supported for .{{ fileExtension || 'unknown' }}</div>
          <p class="mt-1 text-white/75">
            The stream endpoint is available, but Chrome can only play a limited set of containers/codecs directly.
            Use MP4/WebM for browser playback, or open this stream in an external player.
          </p>
          <a class="mt-3 inline-block break-all text-blue-300 hover:text-blue-200" :href="videoUrl" target="_blank" rel="noreferrer">
            {{ videoUrl }}
          </a>
        </div>
      </div>
    </div>
  </Teleport>
</template>
