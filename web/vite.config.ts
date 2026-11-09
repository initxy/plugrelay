import { fileURLToPath, URL } from "node:url";
import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

// `npm run dev` serves the console against a running relay: `make demo` (seeded demo data) by
// default, or any relay via PLUGRELAY_URL.
const relay = process.env.PLUGRELAY_URL ?? "http://127.0.0.1:8787";
// Keep the browser's Host: the relay refuses signed-in writes whose Origin differs from the Host it sees.
const to = { target: relay, changeOrigin: false };

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: { alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) } },
  server: {
    proxy: {
      "/api": to,
      "/v1": to,
      // A browser opening /mcp wants the console's MCP servers page, as the relay itself answers.
      "/mcp": { ...to, bypass: (req) => (req.headers.accept?.includes("text/html") ? req.url : undefined) },
      "/metrics": to,
    },
  },
  build: { outDir: "dist", emptyOutDir: true, sourcemap: false, chunkSizeWarningLimit: 600 },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./src/test/setup.ts"],
    css: false,
  },
});
