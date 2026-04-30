<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue';
import { useTorrentStore } from '../stores/torrentStore';
import DashboardStats from '../components/DashboardStats.vue';
import TorrentTable from '../components/TorrentTable.vue';
import TorrentInspector from '../components/TorrentInspector.vue';
import AddTorrentModal from '../components/AddTorrentModal.vue';

const store = useTorrentStore();
const isAddModalOpen = ref(false);

onMounted(() => {
  store.loadTorrents();
  store.initSSE();
});

onUnmounted(() => {
  store.stopSSE();
});
</script>

<template>
  <div class="flex flex-col gap-6">
    <div class="flex justify-between items-center">
      <h1 class="text-2xl font-bold text-gray-900">Dashboard</h1>
      <button
        @click="isAddModalOpen = true"
        class="inline-flex items-center px-4 py-2 border border-transparent text-sm font-medium rounded-md shadow-sm text-white bg-blue-600 hover:bg-blue-700 focus:outline-none focus:ring-2 focus:ring-offset-2 focus:ring-blue-500"
      >
        Add Torrent
      </button>
    </div>

    <!-- Stats Overview -->
    <DashboardStats />

    <!-- Main Grid -->
    <div class="flex flex-col lg:flex-row gap-6 items-start">
      <div class="flex-1 w-full bg-white shadow rounded-lg border border-gray-200 overflow-hidden">
        <TorrentTable />
      </div>

      <!-- Inspector Sidebar (Right side on large screens) -->
      <div v-if="store.selectedHash" class="w-full lg:w-96 flex-shrink-0 bg-white shadow rounded-lg border border-gray-200 overflow-hidden sticky top-20">
        <TorrentInspector />
      </div>
    </div>

    <AddTorrentModal :is-open="isAddModalOpen" @close="isAddModalOpen = false" />
  </div>
</template>
