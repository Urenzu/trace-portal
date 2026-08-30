import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The built assets are embedded into the Go binary, so they land directly in
// the package that embeds them. Relative asset paths keep the bundle working
// no matter what path the binary serves it from.
export default defineConfig({
  plugins: [react()],
  base: "./",
  build: {
    outDir: "../internal/web/dist",
    emptyOutDir: true,
  },
  server: {
    // In dev, Vite serves the UI and forwards data calls to the running proxy.
    proxy: {
      "/api": "http://127.0.0.1:8317",
    },
  },
});
