<script setup lang="ts">
import BTHealthPanel from '../components/BTHealthPanel.vue';
</script>

<template>
  <div class="flex flex-col gap-6">
    <div>
      <h1 class="text-2xl font-bold text-gray-900">BitTorrent Health</h1>
      <p class="mt-1 text-sm text-gray-500">
        Swarm diagnostics and BitTorrent engine settings without exposing peer addresses.
      </p>
    </div>

    <BTHealthPanel />
  </div>
</template>
