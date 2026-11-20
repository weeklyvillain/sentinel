import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// `npm run dev` proxies API calls to a running sentinel on :8080.
const backend = process.env.SENTINEL_URL ?? "http://localhost:8080";

export default defineConfig({
  plugins: [react()],
  build: {
    outDir: "../internal/web/dist",
    emptyOutDir: true,
    rollupOptions: {
      output: {
        // Libraries change less often than the app, so they get their own
        // long-cached chunks: React + router, and the chart library (recharts
        // with its d3 dependencies), which only chart pages load.
        manualChunks(id) {
          if (!id.includes("node_modules")) return undefined;
          if (/node_modules[\\/](react|react-dom|scheduler|react-router|react-router-dom|@remix-run)[\\/]/.test(id)) return "vendor-react";
          return "vendor-charts";
        },
      },
    },
  },
  server: {
    proxy: {
      "/api": backend,
      "/auth": backend,
      "/github/setup": backend,
    },
  },
});
