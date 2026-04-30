<script setup lang="ts">
import { PlayIcon } from '@heroicons/vue/24/solid';
</script>

<template>
  <div class="min-h-screen bg-gray-50 flex flex-col">
    <!-- Top Navigation -->
    <header class="bg-white shadow-sm border-b border-gray-200 sticky top-0 z-30">
      <div class="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8">
        <div class="flex justify-between h-16 items-center">
          <div class="flex items-center space-x-2">
            <PlayIcon class="h-8 w-8 text-blue-600" />
            <span class="text-xl font-bold text-gray-900 tracking-tight">TorrentStreamHub</span>
          </div>
          <div class="flex items-center space-x-4">
            <a href="https://github.com/anacrolix/torrent" target="_blank" class="text-sm font-medium text-gray-500 hover:text-gray-900 transition">
              About
            </a>
          </div>
        </div>
      </div>
    </header>

    <!-- Main Content -->
    <main class="flex-1 w-full max-w-7xl mx-auto px-4 sm:px-6 lg:px-8 py-8 flex flex-col gap-6">
      <router-view></router-view>
    </main>
  </div>
</template>
