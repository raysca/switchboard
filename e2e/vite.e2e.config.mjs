// Vite config for the e2e stack: serves ../ui unchanged but on E2E ports, with
// /api proxied to the e2e server's admin listener and /ws (voice) to its public listener. Keeps ui/vite.config.ts untouched.
import { fileURLToPath } from "node:url";
import react from "../ui/node_modules/@vitejs/plugin-react/dist/index.js";

const port = Number(process.env.E2E_UI_PORT ?? 15173);
const admin = process.env.E2E_ADMIN_URL ?? "http://127.0.0.1:19590";
const publicUrl = process.env.E2E_PUBLIC_URL ?? "http://127.0.0.1:18580";

export default {
  root: fileURLToPath(new URL("../ui", import.meta.url)),
  plugins: [react()],
  server: { host: "127.0.0.1", port, strictPort: true, proxy: { "/api": admin, "/ws": { target: publicUrl, ws: true } } },
};
