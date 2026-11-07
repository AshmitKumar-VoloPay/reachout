// Timings in one place so tests can shorten them.
export const timing = {
  /** How often the connect screen asks the server whether LinkedIn login finished. */
  connectPollMs: 4000,
  /** How long a connection attempt is followed before the screen gives up. */
  connectTimeoutMs: 10 * 60 * 1000,
  /** How often a running search import is checked. */
  importPollMs: 3000,
  /** Pause after typing before the lead search runs. */
  debounceMs: 300,
}

export const PAGE_SIZE = 50
