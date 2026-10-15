import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import { proxyAuthorization } from "./devProxy";

export default defineConfig({
  plugins: [react()],
  // Dev: management API on the admin listener; the voice test connects to /ws on
  // this origin, which goes to the public listener. `make dev` sets the URLs and the management key.
  // With the key in the environment the proxy signs the dashboard in, so nothing is pasted (the key
  // stays in this process and never reaches the browser); see devProxy.ts for who may use it.
  server: {
    proxy: {
      "/api": {
        target: process.env.GOVOICE_ADMIN_URL ?? "http://127.0.0.1:9090",
        configure: (proxy) => {
          proxy.on("proxyReq", (proxyReq, req) => {
            const authorization = proxyAuthorization(req.headers, process.env.GOVOICE_MANAGEMENT_API_KEY);
            if (authorization) proxyReq.setHeader("Authorization", authorization);
          });
        },
      },
      "/ws": { target: process.env.GOVOICE_PUBLIC_URL ?? "http://127.0.0.1:8080", ws: true },
    },
  },
  test: { environment: "jsdom", setupFiles: ["./src/test/setup.ts"] },
});
