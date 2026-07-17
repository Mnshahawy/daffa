import { fileURLToPath, URL } from 'node:url'
import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import tailwindcss from '@tailwindcss/vite'

// The build lands in ../internal/web/dist/app, where Go embeds it into the binary.
// There is no Node process in production — this output IS the frontend.
//
// Output goes to the app/ SUBDIRECTORY, not dist/ itself, on purpose: emptyOutDir wipes the
// output dir on every build, and the tracked dist/.gitkeep — the placeholder that keeps the
// //go:embed directive valid on a fresh checkout — must not be in the thing that gets wiped.
// So vite owns dist/app/ (volatile, gitignored) and dist/ stays a stable committed directory.
export default defineConfig({
  plugins: [vue(), tailwindcss()],
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  build: {
    outDir: '../internal/web/dist/app',
    emptyOutDir: true,
    // The threshold guards the INITIAL payload — the one chunk a first paint has to wait on.
    // Every route, and the terminal, splits into its own on-demand chunk, so what actually loads
    // up front is `index` (~175 kB), comfortably under this line.
    //
    // The one chunk that blows past 250 kB is xterm, the terminal emulator, and it cannot be made
    // smaller — a VT100 in the browser is simply a lot of code. It is isolated into its own chunk
    // (below) and pulled in ONLY when someone opens an exec or logs terminal, via the
    // defineAsyncComponent import in ContainerView. It never touches the first paint. So the limit
    // is set just above its size rather than to hide it: the warning should still fire for eager
    // bloat, and would, because nothing else comes close.
    chunkSizeWarningLimit: 350,
    rollupOptions: {
      output: {
        manualChunks: {
          // xterm and its addon in one chunk of their own. It changes only when xterm does, so an
          // ordinary app deploy leaves it cached rather than re-downloading a third of a megabyte.
          // Because only the async terminal component imports it, this chunk stays on-demand too.
          xterm: ['@xterm/xterm', '@xterm/addon-fit'],
        },
      },
    },
  },
  server: {
    // `pnpm dev` serves the UI with hot reload and proxies the API to a running
    // `make dev` (default :8099 — 8080 is too crowded a port to assume). Same-origin
    // through the proxy, so the session cookie behaves exactly as it does in prod.
    // Override with DAFFA_PORT=9000 pnpm dev.
    proxy: {
      '/api': { target: `http://localhost:${process.env.DAFFA_PORT ?? 8099}`, changeOrigin: false },
      '/healthz': { target: `http://localhost:${process.env.DAFFA_PORT ?? 8099}` },
    },
  },
})
