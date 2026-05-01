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
});

onUnmounted(() => {
  document.body.style.overflow = '';
});

const handleClose = () => {
  emit('close');
};

const handlePlaybackError = () => {
  playbackError.value = true;
};
</script>

<template>
  <div class="fixed inset-0 z-50 overflow-hidden flex flex-col bg-black">
    <!-- Top bar -->
    <div class="absolute top-0 inset-x-0 h-16 bg-gradient-to-b from-black/80 to-transparent flex justify-between items-center px-4 z-10">
      <h2 class="text-white font-medium text-lg truncate pr-8">{{ title }}</h2>
      <button 
        @click="handleClose" 
        class="text-white/80 hover:text-white p-2 rounded-full hover:bg-white/10 transition"
      >
        <XMarkIcon class="h-8 w-8" />
      </button>
    </div>

    <!-- Video Container -->
    <div class="flex-1 w-full h-full flex items-center justify-center bg-black">
      <video 
        ref="videoPlayer"
        controls 
        autoplay 
        class="w-full h-full max-h-screen object-contain"
        :src="videoUrl"
        @error="handlePlaybackError"
      >
        Your browser does not support the video tag.
      </video>

      <div
        v-if="playbackError || !browserPlayableHint"
        class="absolute bottom-6 left-1/2 w-[calc(100%-2rem)] max-w-2xl -translate-x-1/2 rounded-xl border border-white/10 bg-black/85 p-4 text-sm text-white shadow-2xl"
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
</template>
