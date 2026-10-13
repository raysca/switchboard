import type { CreateFlowInput, ProviderStatus } from "../../api/client";
import { missingEnv } from "../providers/providerStats";

export type Entry = { id: string; label: string; default?: boolean };

export type Stack = {
  provider: ProviderStatus;
  stt: { model: string; label: string; language?: string };
  llm: { model: string; label: string };
  tts: { voice: string; label: string; voices: Entry[] };
  /** Every service of the provider can run now. */
  ready: boolean;
  /** What is still to set on the server when it is not ready. */
  missing: string[];
};

export const DEFAULT_JOB = "Answer calls politely. Greet the caller, find out what they need, and answer in one or two short sentences. If you cannot help, take a message with their name and number.";

const pick = (entries: Entry[] | undefined): Entry | undefined => entries?.find((e) => e.default) ?? entries?.[0];

/** One provider for all three stages, with the catalog's defaults. Mixing providers is done in the flow editor. */
export function stacksFrom(providers: ProviderStatus[]): Stack[] {
  const stacks: Stack[] = [];
  for (const provider of providers) {
    const { stt, llm, tts } = provider.services;
    const sttModel = pick(stt?.models);
    const llmModel = pick(llm?.models);
    const voice = pick(tts?.voices);
    if (!stt || !llm || !tts || !sttModel || !llmModel || !voice) continue;
    stacks.push({
      provider,
      stt: { model: sttModel.id, label: sttModel.label, language: pick(stt.languages)?.id },
      llm: { model: llmModel.id, label: llmModel.label },
      tts: { voice: voice.id, label: voice.label, voices: tts.voices ?? [] },
      ready: stt.available && llm.available && tts.available,
      missing: missingEnv(provider),
    });
  }
  return stacks.sort((a, b) => Number(b.ready) - Number(a.ready) || a.provider.display_name.localeCompare(b.provider.display_name));
}

export function flowFor(stack: Stack, input: { name: string; job: string; voice: string }): CreateFlowInput {
  const provider = stack.provider.id;
  const voice = stack.tts.voices.some((v) => v.id === input.voice) ? input.voice : stack.tts.voice;
  return {
    name: input.name.trim(),
    description: "",
    enabled: true,
    config: {
      stt: { provider, model: stack.stt.model, ...(stack.stt.language ? { language: stack.stt.language } : {}) },
      llm: { provider, model: stack.llm.model, system_prompt: input.job.trim() },
      tts: { provider, voice },
    },
  };
}
