import { useState } from "react";
import type { ProviderStatus } from "../../../api/client";
import { StageFields, type StageValue } from "../StageFields";
import { SettingHelp } from "./SettingHelp";
import { DEFAULT_FAILOVER, FAILOVER_CLASSES, MAX_FALLBACKS, updateStep, type FlowDraft, type DraftStep } from "./model";

type Fallback = { provider: string; model?: string; voice?: string };
type Stage = Record<string, unknown>;
const rec = (v: unknown): Stage => (v && typeof v === "object" && !Array.isArray(v) ? (v as Stage) : {});
const fallbacksOf = (stage: Stage): Fallback[] =>
  Array.isArray(stage.fallbacks) ? stage.fallbacks.map((f) => rec(f) as Fallback).filter((f) => typeof f.provider === "string") : [];
const without = (stage: Stage, key: string): Stage => {
  const { [key]: _drop, ...rest } = stage;
  return rest;
};

const VOICE_SETTINGS = [
  { key: "stability", label: "Stability", min: 0, max: 1 },
  { key: "similarity_boost", label: "Similarity boost", min: 0, max: 1 },
  { key: "style", label: "Style", min: 0, max: 1 },
  { key: "speed", label: "Speed", min: 0.7, max: 1.2 },
] as const;
type VoiceKey = (typeof VOICE_SETTINGS)[number]["key"];

type Props = { graph: FlowDraft; step: DraftStep; providers: ProviderStatus[]; onChange: (g: FlowDraft) => void };

/** Optional depth for one step: fallbacks and tuning. Nothing here is needed to take calls. */
export function StepAdvanced({ graph, step, providers, onChange }: Props) {
  const llm = rec(step.config.llm);
  const tts = rec(step.config.tts);
  const setStage = (cap: "llm" | "tts", stage: Stage) => onChange(updateStep(graph, step.id, { config: { ...step.config, [cap]: stage } }));
  const setFallbacks = (cap: "llm" | "tts", stage: Stage, items: Fallback[]) =>
    setStage(cap, items.length ? { ...stage, fallbacks: items } : without(stage, "fallbacks"));
  const failoverOn = Array.isArray(rec(step.config.failover).on) ? (rec(step.config.failover).on as string[]) : [];
  const failover = failoverOn.length ? failoverOn : DEFAULT_FAILOVER;
  const setFailover = (id: string, on: boolean) => {
    const next = FAILOVER_CLASSES.map((c): string => c.id).filter((c) => (c === id ? on : failover.includes(c)));
    const isDefault = next.length === DEFAULT_FAILOVER.length && DEFAULT_FAILOVER.every((c) => next.includes(c));
    onChange(updateStep(graph, step.id, { config: isDefault ? without(step.config, "failover") : { ...step.config, failover: { on: next } } }));
  };
  const parallel = Number(llm.max_parallel) || 1;
  const voiceSettings = rec(tts.voice_settings) as Partial<Record<VoiceKey, number>>;

  return (
    <>
      <SettingHelp id="fallbacks" />
      <FallbackList
        kind="llm"
        title="Model fallbacks"
        what="model"
        hint="Tried in order if the model stops answering during a call."
        items={fallbacksOf(llm)}
        providers={providers}
        idPrefix={`step-${step.id}-llm-fallback`}
        onChange={(items) => setFallbacks("llm", llm, items)}
      />
      <FallbackList
        kind="tts"
        title="Voice fallbacks"
        what="voice"
        hint="Tried in order if the voice stops answering."
        items={fallbacksOf(tts)}
        providers={providers}
        idPrefix={`step-${step.id}-tts-fallback`}
        onChange={(items) => setFallbacks("tts", tts, items)}
      />
      <fieldset className="stage-box">
        <legend>When to move to a fallback</legend>
        <p className="subtext">Only matters when a stage has fallbacks.</p>
        {FAILOVER_CLASSES.map((c) => (
          <label key={c.id} className="check-row" htmlFor={`step-${step.id}-failover-${c.id}`}>
            <input
              id={`step-${step.id}-failover-${c.id}`}
              type="checkbox"
              checked={failover.includes(c.id)}
              disabled={failover.includes(c.id) && failover.length === 1}
              onChange={(e) => setFailover(c.id, e.target.checked)}
            />
            <span>{c.label}</span>
          </label>
        ))}
        <SettingHelp id="failover.on" />
      </fieldset>
      <fieldset className="stage-box">
        <legend>Tuning</legend>
        <label className="form-field" htmlFor={`step-${step.id}-parallel`}>
          <span>Tool calls at once</span>
          <input
            id={`step-${step.id}-parallel`}
            type="number"
            min={1}
            max={16}
            value={parallel}
            aria-describedby={`step-${step.id}-parallel-hint`}
            onChange={(e) => {
              const n = Math.min(16, Math.max(1, Math.round(Number(e.target.value) || 1)));
              setStage("llm", n > 1 ? { ...llm, max_parallel: n } : without(llm, "max_parallel"));
            }}
          />
        </label>
        <p id={`step-${step.id}-parallel-hint`} className="subtext">
          1 to 16. Higher is faster when one turn needs several lookups.
        </p>
        <SettingHelp id="llm.max_parallel" />
        <label className="check-row" htmlFor={`step-${step.id}-clean`}>
          <input
            id={`step-${step.id}-clean`}
            type="checkbox"
            checked={tts.clean_text !== false}
            onChange={(e) => setStage("tts", e.target.checked ? without(tts, "clean_text") : { ...tts, clean_text: false })}
          />
          <span>Clean text before speaking (strip markdown, links and emoji)</span>
        </label>
        <SettingHelp id="tts.clean_text" />
      </fieldset>
      {tts.provider === "elevenlabs" && (
        <fieldset className="stage-box">
          <legend>Voice settings</legend>
          <p className="subtext">Leave a field empty to keep the ElevenLabs default.</p>
          {VOICE_SETTINGS.map(({ key, label, min, max }) => (
            <label key={key} className="form-field" htmlFor={`step-${step.id}-vs-${key}`}>
              <span>{label}</span>
              <input
                id={`step-${step.id}-vs-${key}`}
                type="number"
                min={min}
                max={max}
                step={0.05}
                defaultValue={voiceSettings[key] ?? ""}
                onChange={(e) => {
                  const next = { ...voiceSettings };
                  if (e.target.value.trim() === "" || !Number.isFinite(Number(e.target.value))) delete next[key];
                  else next[key] = Math.min(max, Math.max(min, Number(e.target.value)));
                  setStage("tts", Object.keys(next).length ? { ...tts, voice_settings: next } : without(tts, "voice_settings"));
                }}
              />
            </label>
          ))}
          <SettingHelp id="tts.voice_settings" />
        </fieldset>
      )}
    </>
  );
}

type ListProps = {
  kind: "llm" | "tts";
  title: string;
  what: "model" | "voice";
  hint: string;
  items: Fallback[];
  providers: ProviderStatus[];
  idPrefix: string;
  onChange: (items: Fallback[]) => void;
};

function FallbackList({ kind, title, what, hint, items, providers, idPrefix, onChange }: ListProps) {
  const [draft, setDraft] = useState<StageValue>({ provider: "" });
  const full = items.length >= MAX_FALLBACKS;
  const ready = !full && !!draft.provider && (kind === "tts" || !!(draft.model ?? "").trim());
  const add = () => {
    onChange([
      ...items,
      kind === "tts" ? { provider: draft.provider, ...(draft.voice ? { voice: draft.voice } : {}) } : { provider: draft.provider, model: draft.model ?? "" },
    ]);
    setDraft({ provider: "" });
  };
  return (
    <fieldset className="stage-box">
      <legend>{title}</legend>
      <p className="subtext">
        {hint} Up to {MAX_FALLBACKS}.
      </p>
      {items.length > 0 && (
        <ol className="plain-list">
          {items.map((f, i) => (
            <li key={`${f.provider}-${f.model ?? f.voice ?? ""}-${i}`} className="row">
              <span className="grow">
                {f.provider}
                {(f.model ?? f.voice) ? ` · ${f.model ?? f.voice}` : ""}
              </span>
              <button type="button" className="action-button-small" onClick={() => onChange(items.filter((_, j) => j !== i))}>
                Remove {what} fallback {i + 1}
              </button>
            </li>
          ))}
        </ol>
      )}
      <StageFields kind={kind} providers={providers} idPrefix={idPrefix} value={draft} onChange={setDraft} />
      <button type="button" className="action-button-small" disabled={!ready} onClick={add}>
        Add {what} fallback
      </button>
    </fieldset>
  );
}
