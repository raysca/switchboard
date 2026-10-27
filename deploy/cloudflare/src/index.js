import { DurableObject } from "cloudflare:workers";

const PORT = 8080;
const INSTANCE = "voice-demo-singleton";
const INACTIVITY_TIMEOUT_MS = 10 * 60 * 1000;
const STARTUP_ATTEMPTS = 60;
const STARTUP_INTERVAL_MS = 1000;

function rejectUntrustedOrigin(request) {
	const origin = request.headers.get("Origin");
	if (!origin) return null; // Native clients may omit Origin; the voice token remains required.

	try {
		const originURL = new URL(origin);
		if (originURL.protocol === "https:" && originURL.host === new URL(request.url).host) return null;
	} catch {
		// Reject malformed Origin values below.
	}
	return new Response("Origin is not allowed", { status: 403 });
}

function forwardedRequest(request) {
	const url = new URL(request.url);
	url.protocol = "http:";
	url.host = "container";
	const forwarded = new Request(url, request);
	forwarded.headers.delete("host");

	// Do not trust client-supplied proxy metadata. The server trusts the internal container hop,
	// so these values must be replaced from Cloudflare's ingress metadata at this boundary.
	forwarded.headers.delete("x-forwarded-for");
	forwarded.headers.delete("x-forwarded-proto");
	forwarded.headers.delete("x-real-ip");
	const clientIP = request.headers.get("CF-Connecting-IP");
	if (clientIP) forwarded.headers.set("X-Forwarded-For", clientIP);
	forwarded.headers.set("X-Forwarded-Proto", "https");
	return forwarded;
}

// The external Postgres profile (deploy/cloudflare/cloudflare-demo-postgres.yaml) is what a
// Cloudflare deployment that runs demos uses: the shipped SQLite file is wiped on container restart,
// so a seeded demo would disappear. Worker secret GOVOICE_DATABASE_DSN switches the profile; the
// persistent SWITCHBOARD_SECRETS_KEY secret enables the encrypted store that seed writes to.
const DEFAULT_DSN = "/tmp/switchboard.db";
const SQLITE_CONFIG = "/etc/govoice/cloudflare-demo.yaml";
const POSTGRES_CONFIG = "/etc/govoice/cloudflare-demo-postgres.yaml";

function databaseConfig(env) {
	const dsn = env.GOVOICE_DATABASE_DSN || DEFAULT_DSN;
	return { dsn, config: /^postgres(ql)?:\/\//.test(dsn) ? POSTGRES_CONFIG : SQLITE_CONFIG };
}

export class SwitchboardContainer extends DurableObject {
	starting;

	async fetch(request) {
		if (new URL(request.url).protocol !== "https:") {
			return new Response("HTTPS is required", { status: 426 });
		}
		const originError = rejectUntrustedOrigin(request);
		if (originError) return originError;

		this.starting ??= this.startAndWaitForHealth().finally(() => {
			this.starting = undefined;
		});
		await this.starting;

		return this.ctx.container.getTcpPort(PORT).fetch(forwardedRequest(request));
	}

	async startAndWaitForHealth() {
		const container = this.ctx.container;
		const database = databaseConfig(this.env);
		if (!container.running) {
			container.start({
				image: container.images.base,
				instance: "standard-2",
				enableInternet: true,
				entrypoint: [
					"/usr/bin/tini",
					"--",
					"/opt/govoice/bin/server",
					"-config",
					database.config,
				],
				env: {
					GOVOICE_DATABASE_DSN: database.dsn,
					SWITCHBOARD_SECRETS_KEY: this.env.SWITCHBOARD_SECRETS_KEY ?? "",
					TOKEN_SECRET: this.env.TOKEN_SECRET,
					GOVOICE_MANAGEMENT_API_KEY: this.env.GOVOICE_MANAGEMENT_API_KEY,
					GOOGLE_API_KEY: this.env.GOOGLE_API_KEY ?? "",
					OPENAI_API_KEY: this.env.OPENAI_API_KEY ?? "",
					OPENAI_BASE_URL: this.env.OPENAI_BASE_URL ?? "",
					XAI_API_KEY: this.env.XAI_API_KEY ?? "",
					DEEPGRAM_API_KEY: this.env.DEEPGRAM_API_KEY ?? "",
					CARTESIA_API_KEY: this.env.CARTESIA_API_KEY ?? "",
					ELEVENLABS_API_KEY: this.env.ELEVENLABS_API_KEY ?? "",
					NVIDIA_API_KEY: this.env.NVIDIA_API_KEY ?? "",
					NVIDIA_STT_FUNCTION_ID: this.env.NVIDIA_STT_FUNCTION_ID ?? "",
					NVIDIA_TTS_FUNCTION_ID: this.env.NVIDIA_TTS_FUNCTION_ID ?? "",
					ORT_DISABLE_TELEMETRY: "1",
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
		throw new Error("Switchboard container did not become healthy on port 8080", {
			cause: lastError,
		});
	}
}

export default {
	fetch(request, env) {
		return env.SWITCHBOARD.getByName(INSTANCE).fetch(request);
	},
};
