import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['./tests/setup.ts'],
    include: ['tests/**/*.test.{ts,tsx}'],
    // An empty run is not a failure. Without this, `pnpm test` exits 1 when no
    // test files match — which breaks CI on any commit that happens to touch
    // only non-tested files, and made this harness fail the moment its own
    // smoke test was removed.
    passWithNoTests: true,
    // Vitest stubs CSS imports to an empty string by default — including
    // `theme.css?raw`. iframeSizing.test.ts reads the stylesheet to check that
    // every <iframe> names a class that actually sizes it, and against an empty
    // stub every such assertion would pass vacuously. Processing CSS makes the
    // raw import return the real file.
    css: true,
  },
})
