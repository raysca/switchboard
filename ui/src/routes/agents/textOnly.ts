import { fromAgentConfig } from "./editor/model";

/** The speech provider that makes a flow text-only (server/registry_silence.go, docs/decisions/silence-provider.md). */
export const TEXT_ONLY_PROVIDER = "silence";

/**
 * A call whose speech-to-text provider is `silence` takes typed turns only, so the console must not ask for a
 * microphone. The call's speech-to-text is the start step's (a flat flow is one step), so this reads a flow
 * version's or a draft's config in either form; anything that is not a flow config is not text-only.
 */
export function isTextOnly(config: unknown): boolean {
  const draft = fromAgentConfig(config);
  const stt = draft.steps.find((s) => s.id === draft.start)?.config.stt;
  return (stt as { provider?: unknown } | null | undefined)?.provider === TEXT_ONLY_PROVIDER;
}
