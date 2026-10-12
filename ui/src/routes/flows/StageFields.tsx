import { useState } from "react";
import type { ProviderStatus } from "../../api/client";

export type StageValue = { provider: string; model?: string; voice?: string; language?: string };
export type StageKind = "stt" | "llm" | "tts";

type Props = {
  kind: StageKind;
  providers: ProviderStatus[];
  value: StageValue;
  onChange: (next: StageValue) => void;
  idPrefix: string;
};

const OTHER = "__other__";

/** Provider, model or voice, and language pickers driven by the server's catalog. */
export function StageFields({ kind, providers, value, onChange, idPrefix }: Props) {
  const upper = kind.toUpperCase();
  const field = kind === "tts" ? "voice" : "model";
  const fieldLabel = kind === "tts" ? "Voice" : `${upper} model`;
  const [otherChosen, setOtherChosen] = useState(false);

  const available = providers.filter((p) => p.services[kind]?.available);
  // No note while providers are still loading (empty list); name the variables once known.
  let note = null;
  if (available.length === 0 && providers.length > 0) {
    const missing = [...new Set(providers.flatMap((p) => p.services[kind]?.missing_env ?? []))];
    note = (
      <p className="subtext" role="note">
        No {upper} provider is available.
        {missing.length > 0 && ` Set one of these on the server and restart it: ${missing.join(", ")}.`}
      </p>
    );
  }

  const svc = providers.find((p) => p.id === value.provider)?.services[kind];
  const entries = (kind === "tts" ? svc?.voices : svc?.models) ?? [];
  const languages = kind === "stt" ? (svc?.languages ?? []) : [];
  const current = value[field] ?? "";
  const known = entries.some((e) => e.id === current);
  const custom = svc?.custom !== false;
  const showOther = custom && !known && (otherChosen || current !== "");

  const pickProvider = (id: string) => {
    setOtherChosen(false);
    const s = providers.find((p) => p.id === id)?.services[kind];
    const dflt = (list?: { id: string; default?: boolean }[]) => list?.find((e) => e.default)?.id;
    const next: StageValue = { provider: id };
    if (kind === "tts") next.voice = dflt(s?.voices);
    else next.model = dflt(s?.models);
    if (kind === "stt") next.language = dflt(s?.languages);
    onChange(next);
  };

  return (
    <>
      {note}
      <label className="form-field" htmlFor={`${idPrefix}-provider`}>
        <span>{upper} provider</span>
        <select id={`${idPrefix}-provider`} value={value.provider} onChange={(e) => pickProvider(e.target.value)}>
          <option value="">Select a provider</option>
          {!available.some((p) => p.id === value.provider) && value.provider && (
            <option value={value.provider}>{value.provider}</option>
          )}
          {available.map((p) => (
            <option key={p.id} value={p.id}>
              {p.display_name}
            </option>
          ))}
        </select>
      </label>
      {value.provider && entries.length > 0 && (
        <label className="form-field" htmlFor={`${idPrefix}-${field}`}>
          <span>{fieldLabel}</span>
          <select
            id={`${idPrefix}-${field}`}
            value={known || (!showOther && current !== "") ? current : showOther ? OTHER : ""}
            onChange={(e) => {
              const v = e.target.value;
              setOtherChosen(v === OTHER);
              onChange({ ...value, [field]: v === OTHER ? "" : v });
            }}
          >
            {!known && !showOther && current === "" && <option value="">Select…</option>}
            {!known && !showOther && current !== "" && <option value={current}>{current}</option>}
            {entries.map((e) => (
              <option key={e.id} value={e.id}>
                {e.label}
              </option>
            ))}
            {custom && <option value={OTHER}>Other…</option>}
          </select>
        </label>
      )}
      {value.provider && ((entries.length > 0 && showOther && custom) || (entries.length === 0 && custom)) && (
        <label className="form-field" htmlFor={`${idPrefix}-${field}-custom`}>
          <span>{entries.length > 0 ? `${fieldLabel} (custom)` : fieldLabel}</span>
          <input
            id={`${idPrefix}-${field}-custom`}
            type="text"
            value={current}
            onChange={(e) => onChange({ ...value, [field]: e.target.value })}
          />
        </label>
      )}
      {languages.length > 0 && (
        <label className="form-field" htmlFor={`${idPrefix}-language`}>
          <span>STT language</span>
          <select
            id={`${idPrefix}-language`}
            value={value.language ?? ""}
            onChange={(e) => onChange({ ...value, language: e.target.value })}
          >
            {!languages.some((l) => l.id === value.language) &&
              (value.language ? <option value={value.language}>{value.language}</option> : <option value="">Select…</option>)}
            {languages.map((l) => (
              <option key={l.id} value={l.id}>
                {l.label}
              </option>
            ))}
          </select>
        </label>
      )}
    </>
  );
}
