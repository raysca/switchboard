import { useState } from "react";
import type { ProviderStatus } from "../../../api/client";
import { StageFields, type StageValue } from "../StageFields";
import { updateNode, type GraphDraft, type GraphNode } from "./model";

type Fallback = { provider: string; model?: string; voice?: string };
type Stage = Record<string, unknown>;
const rec = (v: unknown): Stage => (v && typeof v === "object" && !Array.isArray(v) ? (v as Stage) : {});
const fallbacksOf = (stage: Stage): Fallback[] =>
  Array.isArray(stage.fallbacks) ? stage.fallbacks.map((f) => rec(f) as Fallback).filter((f) => typeof f.provider === "string") : [];
const without = (stage: Stage, key: string): Stage => {
  const { [key]: _drop, ...rest } = stage;
  return rest;
};

type Props = { graph: GraphDraft; step: GraphNode; providers: ProviderStatus[]; onChange: (g: GraphDraft) => void };

/** Optional depth for one step: fallbacks and tuning. Nothing here is needed to take calls. */
export function StepAdvanced({ graph, step, providers, onChange }: Props) {
  const llm = rec(step.config.llm);
  const tts = rec(step.config.tts);
  const setStage = (cap: "llm" | "tts", stage: Stage) => onChange(updateNode(graph, step.id, { config: { ...step.config, [cap]: stage } }));
  const setFallbacks = (cap: "llm" | "tts", stage: Stage, items: Fallback[]) =>
    setStage(cap, items.length ? { ...stage, fallbacks: items } : without(stage, "fallbacks"));
  const parallel = Number(llm.max_parallel) || 1;

  return (
    <>
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
        <label className="check-row" htmlFor={`step-${step.id}-clean`}>
          <input
            id={`step-${step.id}-clean`}
            type="checkbox"
            checked={tts.clean_text !== false}
            onChange={(e) => setStage("tts", e.target.checked ? without(tts, "clean_text") : { ...tts, clean_text: false })}
          />
          <span>Clean text before speaking (strip markdown, links and emoji)</span>
        </label>
      </fieldset>
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
  const ready = !!draft.provider && (kind === "tts" || !!(draft.model ?? "").trim());
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
      <p className="subtext">{hint}</p>
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
