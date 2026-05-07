<script setup lang="ts">
import { ref } from 'vue';
import { PlayIcon } from '@heroicons/vue/24/solid';
import { Bars3Icon, XMarkIcon } from '@heroicons/vue/24/outline';

const isMobileMenuOpen = ref(false);
</script>

<template>
  <div class="min-h-screen bg-gray-50 flex flex-col">
    <!-- Top Navigation -->
    <header class="bg-white shadow-sm border-b border-gray-200 sticky top-0 z-30">
      <div class="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8">
        <div class="flex justify-between h-16 items-center">
          <div class="flex items-center space-x-2">
            <PlayIcon class="h-8 w-8 text-blue-600" />
            <span class="text-xl font-bold text-gray-900 tracking-tight hidden sm:block">TorrentStreamHub</span>
          </div>

          <!-- Desktop Nav -->
          <nav class="hidden md:flex items-center space-x-4">
            <RouterLink
              to="/"
              class="text-sm font-medium text-gray-500 hover:text-gray-900 transition"
              exact-active-class="text-blue-600"
            >
              Dashboard
            </RouterLink>
            <RouterLink
              to="/health/bt"
              class="text-sm font-medium text-gray-500 hover:text-gray-900 transition"
              exact-active-class="text-blue-600"
            >
              BitTorrent Health
            </RouterLink>
            <a href="https://github.com/anacrolix/torrent" target="_blank" class="text-sm font-medium text-gray-500 hover:text-gray-900 transition">
              About
            </a>
          </nav>

          <!-- Mobile Menu Button -->
          <div class="flex items-center md:hidden">
            <button
              @click="isMobileMenuOpen = !isMobileMenuOpen"
              type="button"
              class="inline-flex items-center justify-center p-2 rounded-md text-gray-400 hover:text-gray-500 hover:bg-gray-100 focus:outline-none focus:ring-2 focus:ring-inset focus:ring-blue-500"
            >
              <span class="sr-only">Open main menu</span>
              <Bars3Icon v-if="!isMobileMenuOpen" class="block h-6 w-6" aria-hidden="true" />
              <XMarkIcon v-else class="block h-6 w-6" aria-hidden="true" />
            </button>
          </div>
        </div>
      </div>

      <!-- Mobile Menu -->
      <transition
        enter-active-class="transition ease-out duration-100"
        enter-from-class="transform opacity-0 scale-95"
        enter-to-class="transform opacity-100 scale-100"
        leave-active-class="transition ease-in duration-75"
        leave-from-class="transform opacity-100 scale-100"
        leave-to-class="transform opacity-0 scale-95"
      >
        <div v-if="isMobileMenuOpen" class="md:hidden">
          <div class="px-2 pt-2 pb-3 space-y-1 sm:px-3 bg-white border-b border-gray-200 shadow-lg">
            <RouterLink
              to="/"
              class="block px-3 py-2 rounded-md text-base font-medium text-gray-700 hover:text-gray-900 hover:bg-gray-50"
              exact-active-class="bg-blue-50 text-blue-700"
              @click="isMobileMenuOpen = false"
            >
              Dashboard
            </RouterLink>
            <RouterLink
              to="/health/bt"
              class="block px-3 py-2 rounded-md text-base font-medium text-gray-700 hover:text-gray-900 hover:bg-gray-50"
              exact-active-class="bg-blue-50 text-blue-700"
              @click="isMobileMenuOpen = false"
            >
              BitTorrent Health
            </RouterLink>
            <a 
              href="https://github.com/anacrolix/torrent" 
              target="_blank" 
              class="block px-3 py-2 rounded-md text-base font-medium text-gray-700 hover:text-gray-900 hover:bg-gray-50"
              @click="isMobileMenuOpen = false"
            >
              About
            </a>
          </div>
        </div>
      </transition>
    </header>

    <!-- Main Content -->
    <main class="flex-1 w-full max-w-7xl mx-auto px-4 sm:px-6 lg:px-8 py-8 flex flex-col gap-6">
      <router-view></router-view>
    </main>
  </div>
</template>
