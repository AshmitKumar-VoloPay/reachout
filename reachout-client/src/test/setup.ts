import '@testing-library/jest-dom/vitest'
import { cleanup } from '@testing-library/react'
import { afterEach, beforeEach, vi } from 'vitest'
import { configureApi } from '../api/client'
import { timing } from '../config'

// Fast timers so polling and debouncing finish within a test.
timing.connectPollMs = 15
timing.importPollMs = 15
timing.debounceMs = 10

// jsdom does not implement window.open; the app opens LinkedIn's login page with it.
beforeEach(() => {
  vi.spyOn(window, 'open').mockReturnValue(null)
})

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
  configureApi({ onUnauthorized: undefined })
})
