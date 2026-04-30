<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue';
import { XMarkIcon } from '@heroicons/vue/24/solid';

const props = defineProps<{
  hash: string;
  index: number;
  title: string;
}>();

const emit = defineEmits(['close']);

const videoUrl = ref('');

onMounted(() => {
  // Construct the stream URL
  videoUrl.value = `/api/v1/stream?hash=${props.hash}&index=${props.index}`;
  // Wait, our backend stream URL is `/stream`, not `/api/v1/stream` according to router
  // torrServerHandler is mounted on `/` basically via router.Group without prefix
  videoUrl.value = `/stream?hash=${props.hash}&index=${props.index}`;
  
  // Disable body scroll when modal is open
  document.body.style.overflow = 'hidden';
});

onUnmounted(() => {
  // Re-enable body scroll
  document.body.style.overflow = '';
});

const handleClose = () => {
  emit('close');
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
      >
        Your browser does not support the video tag.
      </video>
    </div>
  </div>
</template>
