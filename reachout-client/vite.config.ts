import react from '@vitejs/plugin-react'
import { defineConfig } from 'vitest/config'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  server: {
    // Go API (reachout-server) listens on :8080
    proxy: { '/api': 'http://localhost:8080' },
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
    // The live check needs a running server; run it with `npm run test:live`.
    exclude: ['node_modules', 'dist', 'e2e/**'],
  },
})
