<script setup lang="ts">
defineProps<{
  isOpen: boolean;
  title: string;
  message: string;
  confirmLabel: string;
  danger?: boolean;
  loading?: boolean;
}>();

const emit = defineEmits(['confirm', 'cancel']);
</script>

<template>
  <Teleport to="body">
    <div v-if="isOpen" class="fixed inset-0" style="z-index: 8000" role="dialog" aria-modal="true">
      <div class="fixed inset-0 bg-gray-900/60" @click="!loading && emit('cancel')"></div>

      <div class="fixed inset-0 flex items-center justify-center p-4">
        <div class="w-full max-w-md rounded-2xl bg-white p-6 text-left shadow-2xl ring-1 ring-black/5">
          <h3 class="text-lg font-semibold text-gray-900">{{ title }}</h3>
          <p class="mt-3 whitespace-pre-line text-sm leading-6 text-gray-600">{{ message }}</p>

          <div class="mt-6 flex flex-col-reverse gap-3 sm:flex-row sm:justify-end">
            <button
              type="button"
              :disabled="loading"
              class="inline-flex justify-center rounded-md border border-gray-300 bg-white px-4 py-2 text-sm font-medium text-gray-700 shadow-sm transition hover:bg-gray-50 disabled:opacity-50"
              @click="emit('cancel')"
            >
              Cancel
            </button>
            <button
              type="button"
              :disabled="loading"
              :class="[
                'inline-flex justify-center rounded-md border border-transparent px-4 py-2 text-sm font-semibold text-white shadow-sm transition disabled:opacity-50',
                danger ? 'bg-red-600 hover:bg-red-700' : 'bg-blue-600 hover:bg-blue-700'
              ]"
              @click="emit('confirm')"
            >
              {{ loading ? 'Working...' : confirmLabel }}
            </button>
          </div>
        </div>
      </div>
    </div>
  </Teleport>
</template>
