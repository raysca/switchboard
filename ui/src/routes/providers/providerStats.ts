import type { AgentDTO, ProviderStatus } from "../../api/client";

export const SERVICES = ["stt", "llm", "tts"] as const;
export type Service = (typeof SERVICES)[number];
export const SERVICE_NAMES: Record<Service, string> = { stt: "Speech-to-text", llm: "Language models", tts: "Text-to-speech" };

export function hasService(p: ProviderStatus, s: Service): boolean {
  return !!p.services[s];
}

/** "STT · LLM · TTS": the services the catalog lists for the provider. */
export function serviceList(p: ProviderStatus): string {
  return SERVICES.filter((s) => hasService(p, s))
    .map((s) => s.toUpperCase())
    .join(" · ");
}

/** The server reads keys from its environment only; there is no keyring to store one in yet (spec S3). */
export function credential(p: ProviderStatus): { text: string; ok: boolean } {
  return p.detected ? { text: "Environment variable", ok: true } : { text: "Not configured", ok: false };
}

/** The variables still to set: named by the provider's services (once each), else the provider's own. */
export function missingEnv(p: ProviderStatus): string[] {
  const named = [...new Set(SERVICES.flatMap((s) => p.services[s]?.missing_env ?? []))];
  if (named.length > 0) return named;
  return p.detected ? [] : p.env;
}

/** What one service can do now, or what it still needs. `null` when the catalog has no such service. */
export function serviceState(p: ProviderStatus, s: Service): { state: "ok" | "missing" | "absent"; text: string } | null {
  const svc = p.services[s];
  if (!svc) return null;
  if (svc.available) return { state: "ok", text: "available" };
  if (svc.reason === "missing_env") return { state: "missing", text: `needs ${(svc.missing_env ?? []).join(", ")}` };
  return { state: "absent", text: "not in this build" };
}

/** Per listed service: the models (voices for TTS), then the languages. */
export function catalogLines(p: ProviderStatus): { service: Service; text: string }[] {
  return SERVICES.flatMap((s) => {
    const svc = p.services[s];
    if (!svc) return [];
    const names = (s === "tts" ? svc.voices : svc.models) ?? [];
    const parts = [names.map((n) => n.label).join(", "), (svc.languages ?? []).map((l) => l.label).join(", ")].filter(Boolean);
    return [{ service: s, text: parts.join(" · ") || "No catalog entries" }];
  });
}

type Stage = { provider?: unknown; fallbacks?: unknown };
type Block = Partial<Record<Service, Stage>>;

/** The provider each stage names, and its fallbacks: in a flat config, or in every step of a graph. */
function stageProviders(config: unknown): string[] {
  const root = (config ?? {}) as { nodes?: { config?: Block }[] } & Block;
  const blocks: Block[] = Array.isArray(root.nodes) ? root.nodes.map((n) => n?.config ?? {}) : [root];
  return blocks
    .flatMap((b) => SERVICES.map((s) => b[s] as Stage | undefined))
    .flatMap((stage) => [stage?.provider, ...(Array.isArray(stage?.fallbacks) ? stage.fallbacks.map((f: Stage | null) => f?.provider) : [])])
    .filter((p): p is string => typeof p === "string" && p !== "");
}

/** Flows whose current version uses the provider in any stage or step. Archived flows never count. */
export function flowsUsing(flows: AgentDTO[], providerId: string): AgentDTO[] {
  return flows.filter((f) => !f.archived && stageProviders(f.current_version?.config).includes(providerId));
}
