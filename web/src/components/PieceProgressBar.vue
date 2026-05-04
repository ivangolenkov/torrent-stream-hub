<script setup lang="ts">
import { ref, watch, onMounted, onUnmounted } from 'vue';
import type { Torrent, File } from '../types';

const props = defineProps<{
  file: File;
  torrent: Torrent;
  piecesString: string;
}>();

const canvasRef = ref<HTMLCanvasElement | null>(null);
const wrapperRef = ref<HTMLElement | null>(null);
let resizeObserver: ResizeObserver | null = null;

const drawPieces = () => {
  if (!canvasRef.value || !wrapperRef.value || !props.torrent.piece_length || !props.piecesString) {
    return;
  }

  const canvas = canvasRef.value;
  const ctx = canvas.getContext('2d');
  if (!ctx) return;

  // Make canvas size match visual size to avoid blurriness
  const rect = wrapperRef.value.getBoundingClientRect();
  canvas.width = rect.width;
  canvas.height = rect.height;

  const fileOffset = props.file.offset;
  const fileSize = props.file.size;
  const pieceLength = props.torrent.piece_length;
  
  if (fileSize === 0 || pieceLength === 0) {
    // Empty file
    ctx.fillStyle = '#f3f4f6'; // gray-100
    ctx.fillRect(0, 0, canvas.width, canvas.height);
    return;
  }

  // Calculate which pieces belong to this file
  const firstPieceIdx = Math.floor(fileOffset / pieceLength);
  const lastPieceIdx = Math.floor((fileOffset + fileSize - 1) / pieceLength);
  const numPiecesInFile = lastPieceIdx - firstPieceIdx + 1;

  // Clear canvas
  ctx.clearRect(0, 0, canvas.width, canvas.height);

  // Background (missing pieces)
  ctx.fillStyle = '#e5e7eb'; // gray-200
  ctx.fillRect(0, 0, canvas.width, canvas.height);

  // If pieces string doesn't cover this file yet, just return the gray background
  if (!props.piecesString || props.piecesString.length <= firstPieceIdx) {
    // If the file is 0% downloaded, it's correct to show gray.
    // However, if the file has some downloaded bytes but piecesString is empty,
    // we should show a fallback (e.g. a simple progress bar based on bytes).
    if (props.file.downloaded > 0 && fileSize > 0) {
       ctx.fillStyle = '#3b82f6'; // blue-500
       const progress = props.file.downloaded / fileSize;
       ctx.fillRect(0, 0, canvas.width * progress, canvas.height);
    }
    return;
  }

  // Draw pieces
  const pieceWidth = canvas.width / numPiecesInFile;
  
  for (let i = 0; i < numPiecesInFile; i++) {
    const globalPieceIdx = firstPieceIdx + i;
    
    // Safety check
    if (globalPieceIdx >= props.piecesString.length) break;
    
    const state = props.piecesString[globalPieceIdx];
    
    if (state === '2') {
      ctx.fillStyle = '#3b82f6'; // blue-500 (completed)
    } else if (state === '1') {
      ctx.fillStyle = '#22c55e'; // green-500 (downloading)
    } else {
      continue; // keep gray
    }

    // To prevent subpixel rendering gaps, we add a tiny overlap (0.5px) if it's not the last piece
    const width = (i === numPiecesInFile - 1) ? pieceWidth : pieceWidth + 0.5;
    // Fix: Add a tiny negative offset to x to cover gaps left by subpixel rendering of previous pieces
    const x = Math.max(0, i * pieceWidth - 0.2);
    ctx.fillRect(x, 0, width + 0.2, canvas.height);
  }
};

watch(() => props.piecesString, drawPieces);
watch(() => props.file.downloaded, drawPieces); // Fallback redraw if downloaded changes

onMounted(() => {
  drawPieces();
  
  // Redraw when container resizes
  if (wrapperRef.value) {
    resizeObserver = new ResizeObserver(() => {
      // Use requestAnimationFrame to avoid "ResizeObserver loop limit exceeded"
      requestAnimationFrame(drawPieces);
    });
    resizeObserver.observe(wrapperRef.value);
  }
});

onUnmounted(() => {
  if (resizeObserver) {
    resizeObserver.disconnect();
  }
});
</script>

<template>
  <div ref="wrapperRef" class="w-full h-4 rounded overflow-hidden relative" :title="`Pieces mapping`">
    <canvas ref="canvasRef" class="absolute inset-0 w-full h-full block"></canvas>
  </div>
</template>