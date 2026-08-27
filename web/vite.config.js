import { defineConfig } from "vite";

export default defineConfig({
  build: {
    outDir: "dist",
    // Small app, few modules — inlining the sourcemap would double the payload
    // for no benefit, but a separate one costs nothing to ship.
    sourcemap: true,
  },
  server: {
    port: 5173,
    // `npm run dev` against a locally running server, so the frontend can be
    // iterated on without rebuilding the container.
    proxy: {
      "/api": "http://localhost:8080",
      "/healthz": "http://localhost:8080",
    },
  },
});
