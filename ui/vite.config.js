import { defineConfig } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';

// The single shared bundle is served EMBEDDED at same origin by both backends,
// always mounted at /ui (Go: http.StripPrefix("/ui"); Node: dist/ui under /ui).
//
// base: '/ui/' makes asset URLs absolute (/ui/assets/…) so they resolve
// correctly whether the page is opened at /ui or /ui/ — a relative base breaks
// on /ui (no trailing slash), where ./assets resolves against / and 404s.
//
// modulePreload.polyfill: false drops Vite's inline preload-polyfill <script>, so
// the strict UI CSP (script-src 'self', no 'unsafe-inline') is satisfied by the
// external hashed bundle alone — see go/internal/web + node/src/web.
export default defineConfig({
  base: '/ui/',
  plugins: [svelte()],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    modulePreload: { polyfill: false },
  },
  // Dev-only convenience: proxy the API surface to a locally running bridge
  // (default HTTP_PORT 9090) so `npm run dev` can talk to /health + /Calls.json.
  // Irrelevant to the embedded production build.
  server: {
    proxy: {
      '/health': 'http://127.0.0.1:9090',
      '/2010-04-01': 'http://127.0.0.1:9090',
    },
  },
});
