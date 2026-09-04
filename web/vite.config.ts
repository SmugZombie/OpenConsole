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
    rollupOptions: {
      // Two pages: the client, and the explainer the footer links to. The
      // explainer is a separate document rather than a route in the app so it
      // needs no JavaScript to be readable, and so a browser can find it.
      // Paths are relative to the project root, which keeps this config free
      // of node typings it would otherwise need only for __dirname.
      input: {
        main: 'index.html',
        docs: 'docs.html',
      },
    },
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
