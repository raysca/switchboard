import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  // Dev: management API on the admin listener; the voice test connects to /ws on
  // this origin, which goes to the public listener.
  server: {
    proxy: {
      "/api": "http://127.0.0.1:9090",
      "/ws": { target: "http://127.0.0.1:8080", ws: true },
    },
  },
  test: { environment: "jsdom", setupFiles: ["./src/test/setup.ts"] },
});
