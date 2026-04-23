import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Spotify rejects `http://localhost` redirect URIs since late 2024 — only
// `https://` or the loopback literal `http://127.0.0.1` are accepted. Pinning
// host to 127.0.0.1 keeps cookies, redirect-URIs and proxy-target on one
// consistent origin in dev.
const BACKEND_URL = process.env.VITE_BACKEND_URL ?? "http://127.0.0.1:8080";

export default defineConfig({
  plugins: [react()],
  server: {
    host: "127.0.0.1",
    port: 5173,
    proxy: {
      "/api": {
        target: BACKEND_URL,
        changeOrigin: true,
      },
      "/healthz": {
        target: BACKEND_URL,
        changeOrigin: true,
      },
    },
  },
});
