import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { release } from "node:os";

// Under WSL the checkout usually sits on a Windows drive, and inotify does not
// fire across that mount: file changes reach the disk but never reach Vite, so
// HMR silently stops working. Polling is the only thing the mount supports.
const onWSL = process.platform === "linux" && /microsoft/i.test(release());

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
    // Only on WSL: native watching works everywhere else, and polling this
    // often costs CPU for nothing.
    watch: onWSL ? { usePolling: true, interval: 300 } : undefined,
  },
});
