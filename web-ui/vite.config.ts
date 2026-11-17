import { defineConfig } from "vite";
import { fileURLToPath, URL } from "node:url";
import vue from "@vitejs/plugin-vue";
import tailwindcss from "@tailwindcss/vite";

// Dev: proxy /api/* to the natively-running brain (WEB_UI.md dev loop). SSE
// streams pass through the same proxy. In production Caddy does this routing.
const BRAIN = process.env.MOOSE_BRAIN ?? "http://localhost:8080";

export default defineConfig({
  plugins: [vue(), tailwindcss()],
  resolve: {
    // "@" → src/, matching the shadcn-vue alias convention so the CLI can add
    // components later without path rewrites.
    alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) },
  },
  server: {
    port: 5173,
    // make dev HOSTED=1 serves the dashboard through the dev Caddy at
    // <box>.onmoose.io (ENVIRONMENT.md # Networking & discovery), so Vite has to
    // listen where the Caddy container can reach it and accept that host name.
    // Unset, Vite stays on localhost as before.
    host: process.env.MOOSE_UI_HOST || undefined,
    allowedHosts: [".onmoose.io"],
    proxy: {
      "/api": {
        target: BRAIN,
        changeOrigin: true,
        // SSE: disable buffering so events arrive promptly.
        configure: (proxy) => {
          proxy.on("proxyReq", (proxyReq) => proxyReq.setHeader("X-Forwarded-By", "vite"));
        },
      },
    },
  },
});
