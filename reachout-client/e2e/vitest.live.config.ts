import { defineConfig } from 'vitest/config'

// Runs the real API client against a real server (see scripts/live-check.sh).
export default defineConfig({
  test: { environment: 'node', include: ['e2e/*.test.ts'], testTimeout: 60_000, fileParallelism: false },
})
