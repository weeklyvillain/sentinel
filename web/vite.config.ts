import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// `npm run dev` proxies API calls to a running sentinel on :8080.
const backend = process.env.SENTINEL_URL ?? "http://localhost:8080";

export default defineConfig({
  plugins: [react()],
  build: {
    outDir: "../internal/web/dist",
    emptyOutDir: true,
    chunkSizeWarningLimit: 1200,
  },
  server: {
    proxy: {
      "/api": backend,
      "/auth": backend,
      "/github/setup": backend,
    },
  },
});
