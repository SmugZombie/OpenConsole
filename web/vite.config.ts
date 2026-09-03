import { defineConfig } from 'vite';

// The bundle is embedded into the relay binary, so it builds straight into the
// Go package that embeds it.
//
// `base` is absolute because the terminal page is served at /s/<session>, and
// relative asset paths would resolve against that prefix and 404.
export default defineConfig({
  base: '/',
  build: {
    outDir: '../internal/webui/dist',
    emptyOutDir: true,
    // The relay serves these itself; a sourcemap would ship the whole client
    // source inside the binary for no benefit to an operator.
    sourcemap: false,
    target: 'es2022',
  },
  server: {
    // `npm run dev` serves the UI with hot reload and forwards API and tunnel
    // traffic to a relay running separately. ws:true is what makes the
    // WebSocket upgrade pass through.
    proxy: {
      '/api': { target: 'http://localhost:8080', ws: true, changeOrigin: true },
      '/health': { target: 'http://localhost:8080', changeOrigin: true },
    },
  },
});
