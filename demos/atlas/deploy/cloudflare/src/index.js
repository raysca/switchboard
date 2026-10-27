import { DurableObject } from "cloudflare:workers";

// Worker + container for the Atlas Travel Concierge demo MCP server. The Worker proxies only /mcp,
// /api/* and /health to one demo container and passes the bearer token through; the MCP process
// validates it. The token is the ATLAS_MCP_TOKEN Wrangler secret (never generated here, never in
// this file). The container sets ATLAS_MCP_ADDR=0.0.0.0:$PORT because the binary binds loopback by
// default and the /api fixtures must be reachable through the Worker.
const PORT = 8081;
const INSTANCE = "atlas-mcp-singleton";
const INACTIVITY_TIMEOUT_MS = 10 * 60 * 1000;
const STARTUP_ATTEMPTS = 60;
const STARTUP_INTERVAL_MS = 1000;

function allowedPath(pathname) {
	return pathname === "/mcp" || pathname === "/health" || pathname.startsWith("/api/");
}

function forwardedRequest(request) {
	const url = new URL(request.url);
	url.protocol = "http:";
	url.host = "container";
	const forwarded = new Request(url, request);
	forwarded.headers.delete("host");
	return forwarded;
}

export class AtlasMCPContainer extends DurableObject {
	starting;

	async fetch(request) {
		this.starting ??= this.startAndWaitForHealth().finally(() => {
			this.starting = undefined;
		});
		await this.starting;
		return this.ctx.container.getTcpPort(PORT).fetch(forwardedRequest(request));
	}

	async startAndWaitForHealth() {
		const container = this.ctx.container;
		if (!container.running) {
			if (!this.env.ATLAS_MCP_TOKEN) {
				throw new Error("set the ATLAS_MCP_TOKEN Wrangler secret before deploying");
			}
			container.start({
				image: container.images.base,
				instance: "standard-1",
				enableInternet: false,
				env: {
					ATLAS_MCP_TOKEN: this.env.ATLAS_MCP_TOKEN,
					PORT: String(PORT),
					ATLAS_MCP_ADDR: `0.0.0.0:${PORT}`,
				},
			});
		}
		await container.setInactivityTimeout(INACTIVITY_TIMEOUT_MS);

		const health = container.getTcpPort(PORT);
		let lastError;
		for (let attempt = 0; attempt < STARTUP_ATTEMPTS; attempt++) {
			try {
				const response = await health.fetch("http://container/health", {
					signal: AbortSignal.timeout(2000),
				});
				await response.body?.cancel();
				if (!response.ok) throw new Error(`Health returned ${response.status}`);
				return;
			} catch (error) {
				lastError = error;
				await scheduler.wait(STARTUP_INTERVAL_MS);
			}
		}
		throw new Error("atlas-mcp container did not become healthy on port 8081", {
			cause: lastError,
		});
	}
}

export default {
	fetch(request, env) {
		if (!allowedPath(new URL(request.url).pathname)) {
			return new Response("Not found", { status: 404 });
		}
		return env.MCP.getByName(INSTANCE).fetch(request);
	},
};
