import { useState } from "react";
import type { ProviderStatus, ServerConfigDTO } from "../../../api/client";
import { SidePanel } from "../../../components/SidePanel";
import { StageFields, type StageValue } from "../StageFields";
import { AudioFields, QuietCallerBasics, QuietCallerDetails } from "./CallBehaviorFields";
import { SettingHelp } from "./SettingHelp";
import { flowIssues, MAX_SUMMARY_PROMPT_BYTES, STT_P99_RANGE, updateStep, type FlowDraft, type StepConfig } from "./model";

type Rec = Record<string, unknown>;
const rec = (v: unknown): Rec => (v && typeof v === "object" && !Array.isArray(v) ? (v as Rec) : {});
const str = (v: unknown): string => (typeof v === "string" ? v : "");
const without = (o: Rec, key: string): Rec => {
  const { [key]: _drop, ...rest } = o;
  return rest;
};
/** An empty number field removes the key: the server default applies. */
const numberOrNothing = (raw: string): number | null => (raw.trim() === "" ? null : Math.round(Number(raw)));

export type FlowSettingsPanelProps = {
  /** The published version, shown read-only: what is live, and its content hash. */
  published?: { number: number; createdAt: string; hash: string };
  /** Publish the previous version again as the next one. Absent when there is nothing to go back to. */
  onRestore?: () => Promise<void>;
  graph: FlowDraft;
  providers: ProviderStatus[];
  /** What the server offers and defaults to; absent while it loads or if it cannot be read. */
  serverConfig?: ServerConfigDTO;
  name: string;
  description: string;
  onName: (v: string) => void;
  onDescription: (v: string) => void;
  onChange: (g: FlowDraft) => void;
  onClose: () => void;
  returnFocusTo?: HTMLElement | null;
  fallbackFocusId?: string;
};

/** What the whole call shares. Stored on the start step (server: call-level config). */
export function FlowSettingsPanel({ graph, providers, serverConfig, name, description, onName, onDescription, onChange, onClose, returnFocusTo, fallbackFocusId, published, onRestore }: FlowSettingsPanelProps) {
  const [restoring, setRestoring] = useState(false);
  const [restoreError, setRestoreError] = useState<string | null>(null);
  const start = graph.steps.find((n) => n.id === graph.start);
  const cfg: StepConfig = start?.config ?? {};
  const stt = rec(cfg.stt);
  const context = rec(cfg.context);
  const recording = rec(cfg.recording);
  const problems = flowIssues(graph);
  const advancedSet = (["vad_stop_ms", "stt_p99_ms", "context", "audio"] as const).filter((k) => k in cfg).length +
    (Object.keys(rec(cfg.idle)).some((k) => k !== "user_timeout_ms") ? 1 : 0) +
    (graph.route_cue && graph.routes.length > 0 ? 1 : 0);
  const [open, setOpen] = useState(advancedSet > 0);
  // Everything but the essentials' own messages belongs to a field inside Advanced.
  const advancedProblem = problems.some((p) => !/^(Choose |Confirm that callers)/.test(p));
  const setKey = (key: string, value: unknown) => {
    if (!start) return;
    onChange(updateStep(graph, start.id, { config: value === null || value === undefined ? without(cfg, key) : { ...cfg, [key]: value } }));
  };
  const summary = rec(context.summarization);
  const setSummary = (key: string, value: unknown) => setContext({ ...context, summarization: value === null ? without(summary, key) : { ...summary, [key]: value } });
  const setContext = (next: Rec) => setKey("context", Object.keys(next).length ? next : null);
  const cue = graph.route_cue;

  return (
    <SidePanel
      title="Flow settings"
      onClose={onClose}
      returnFocusTo={returnFocusTo}
      fallbackFocusId={fallbackFocusId}
      footer={
        <>
          <span className="subtext grow">Part of the draft until you publish.</span>
          <button type="button" className="action-button" onClick={onClose}>
            Done
          </button>
        </>
      }
    >
      {problems.length > 0 && (
        <div role="note" className="note note--bad">
          <ul className="plain-list">
            {problems.map((p) => (
              <li key={p}>{p}</li>
            ))}
          </ul>
        </div>
      )}
      <p className="subtext">Shared by every step for the whole call.</p>
      <label className="form-field" htmlFor="flow-name">
        <span>Flow name</span>
        <input id="flow-name" type="text" maxLength={128} value={name} onChange={(e) => onName(e.target.value)} />
      </label>
      <label className="form-field" htmlFor="flow-description">
        <span>Description</span>
        <textarea id="flow-description" rows={2} maxLength={1024} value={description} onChange={(e) => onDescription(e.target.value)} />
      </label>

      {!start ? (
        <p className="subtext">Add a step first; these settings live on the start step.</p>
      ) : (
        <>
          <fieldset className="stage-box">
            <legend>Listening</legend>
            <StageFields
              kind="stt"
              providers={providers}
              idPrefix="flow-stt"
              value={{ provider: str(stt.provider), model: str(stt.model), language: str(stt.language) }}
              onChange={(v: StageValue) => setKey("stt", { ...stt, provider: v.provider, model: v.model ?? "", language: v.language ?? "" })}
            />
          </fieldset>
          <fieldset className="stage-box">
            <legend>Recording</legend>
            <label className="check-row" htmlFor="flow-record">
              <input
                id="flow-record"
                type="checkbox"
                checked={recording.enabled === true}
                onChange={(e) => setKey("recording", e.target.checked ? { enabled: true, disclosure_confirmed: recording.disclosure_confirmed === true } : null)}
              />
              <span>Record calls</span>
            </label>
            <label className="check-row" htmlFor="flow-disclosure">
              <input
                id="flow-disclosure"
                type="checkbox"
                disabled={recording.enabled !== true}
                checked={recording.disclosure_confirmed === true}
                onChange={(e) => setKey("recording", { enabled: true, disclosure_confirmed: e.target.checked })}
              />
              <span>Callers are told the call is recorded</span>
            </label>
            <SettingHelp id="recording" />
          </fieldset>
          <QuietCallerBasics cfg={cfg} setKey={setKey} serverConfig={serverConfig} />
        </>
      )}
      {start && (
        <details className="advanced" open={open || advancedProblem} onToggle={(e) => setOpen(e.currentTarget.open)}>
          <summary>{advancedSet > 0 ? `Advanced · ${advancedSet} set` : "Advanced"}</summary>
          <QuietCallerDetails cfg={cfg} setKey={setKey} serverConfig={serverConfig} />
          <AudioFields cfg={cfg} setKey={setKey} serverConfig={serverConfig} />
          <fieldset className="stage-box">
            <legend>Turn-taking</legend>
            <label className="form-field" htmlFor="flow-vad">
              <span>Wait after the caller stops (ms)</span>
              <input
                id="flow-vad"
                type="number"
                min={200}
                max={2000}
                placeholder="Server default"
                aria-describedby="flow-vad-hint"
                value={typeof cfg.vad_stop_ms === "number" ? cfg.vad_stop_ms : ""}
                onChange={(e) => setKey("vad_stop_ms", numberOrNothing(e.target.value))}
              />
            </label>
            <p id="flow-vad-hint" className="subtext">
              200 to 2000. Shorter replies sooner; longer interrupts the caller less.
            </p>
            <SettingHelp id="vad_stop_ms" />
            <label className="form-field" htmlFor="flow-stt-p99">
              <span>Wait for a late transcript (ms)</span>
              <input
                id="flow-stt-p99"
                type="number"
                min={STT_P99_RANGE[0]}
                max={STT_P99_RANGE[1]}
                placeholder="Server default"
                aria-describedby="flow-stt-p99-hint"
                value={typeof cfg.stt_p99_ms === "number" ? cfg.stt_p99_ms : ""}
                onChange={(e) => setKey("stt_p99_ms", numberOrNothing(e.target.value))}
              />
            </label>
            <p id="flow-stt-p99-hint" className="subtext">
              {STT_P99_RANGE[0]} to {STT_P99_RANGE[1]}. Empty: the server default.
            </p>
            <SettingHelp id="stt_p99_ms" />
          </fieldset>
          <fieldset className="stage-box">
            <legend>Memory</legend>
            <div className="grid-2">
              <label className="form-field" htmlFor="flow-max-messages">
                <span>Max messages</span>
                <input
                  id="flow-max-messages"
                  type="number"
                  min={2}
                  placeholder="Server default"
                  value={typeof context.max_messages === "number" ? context.max_messages : ""}
                  onChange={(e) => {
                    const n = numberOrNothing(e.target.value);
                    setContext(n === null ? without(context, "max_messages") : { ...context, max_messages: n });
                  }}
                />
              </label>
              <label className="form-field" htmlFor="flow-max-tokens">
                <span>Max tokens</span>
                <input
                  id="flow-max-tokens"
                  type="number"
                  min={1}
                  placeholder="Server default"
                  value={typeof context.max_tokens === "number" ? context.max_tokens : ""}
                  onChange={(e) => {
                    const n = numberOrNothing(e.target.value);
                    setContext(n === null ? without(context, "max_tokens") : { ...context, max_tokens: n });
                  }}
                />
              </label>
            </div>
            <SettingHelp id="context.limits" />
            <label className="check-row" htmlFor="flow-summarise">
              <input
                id="flow-summarise"
                type="checkbox"
                checked={rec(context.summarization).enabled === true}
                onChange={(e) => setContext(e.target.checked ? { ...context, summarization: { enabled: true } } : without(context, "summarization"))}
              />
              <span>Summarise older turns</span>
            </label>
            <SettingHelp id="summarization.enabled" />
            {summary.enabled === true && (
              <>
                <label className="form-field" htmlFor="flow-summary-trigger">
                  <span>Summarise when history reaches (tokens)</span>
                  <input
                    id="flow-summary-trigger"
                    type="number"
                    min={1}
                    placeholder="Three quarters of Max tokens"
                    aria-describedby="flow-summary-trigger-hint"
                    value={typeof summary.trigger_tokens === "number" ? summary.trigger_tokens : ""}
                    onChange={(e) => setSummary("trigger_tokens", numberOrNothing(e.target.value))}
                  />
                </label>
                <p id="flow-summary-trigger-hint" className="subtext">
                  Empty: three quarters of Max tokens. Must be below Max tokens.
                </p>
                <SettingHelp id="summarization.trigger_tokens" />
                <label className="form-field" htmlFor="flow-summary-keep">
                  <span>Keep the latest turns</span>
                  <input
                    id="flow-summary-keep"
                    type="number"
                    min={1}
                    placeholder="4"
                    aria-describedby="flow-summary-keep-hint"
                    value={typeof summary.keep_turns === "number" ? summary.keep_turns : ""}
                    onChange={(e) => setSummary("keep_turns", numberOrNothing(e.target.value))}
                  />
                </label>
                <p id="flow-summary-keep-hint" className="subtext">
                  Empty: 4.
                </p>
                <SettingHelp id="summarization.keep_turns" />
                <label className="form-field" htmlFor="flow-summary-prompt">
                  <span>Summary instructions</span>
                  <textarea
                    id="flow-summary-prompt"
                    rows={3}
                    placeholder="Built-in instructions"
                    aria-describedby="flow-summary-prompt-hint"
                    value={typeof summary.prompt === "string" ? summary.prompt : ""}
                    onChange={(e) => setSummary("prompt", e.target.value === "" ? null : e.target.value)}
                  />
                </label>
                <p id="flow-summary-prompt-hint" className="subtext">
                  Empty: the built-in prompt. At most {MAX_SUMMARY_PROMPT_BYTES} bytes.
                </p>
                <SettingHelp id="summarization.prompt" />
              </>
            )}
          </fieldset>
      {graph.routes.length > 0 && (
        <fieldset className="stage-box">
          <legend>Route sound</legend>
          <p className="subtext">Plays while the caller moves between steps. Sounds are set up on the server (audio.cues).</p>
          <div className="grid-2">
            <label className="form-field" htmlFor="cue-name">
              <span>Sound name</span>
              <input
                id="cue-name"
                type="text"
                placeholder="none"
                value={cue?.name ?? ""}
                onChange={(e) => {
                  const v = e.target.value.trim();
                  const { route_cue: _drop, ...rest } = graph;
                  onChange(v ? { ...rest, route_cue: { ...(cue ?? {}), name: v } } : rest);
                }}
              />
            </label>
            <label className="form-field" htmlFor="cue-after">
              <span>Start after (ms)</span>
              <input
                id="cue-after"
                type="number"
                min={0}
                max={10000}
                disabled={!cue}
                value={cue?.after_ms ?? ""}
                onChange={(e) => {
                  if (!cue) return;
                  const n = numberOrNothing(e.target.value);
                  onChange({ ...graph, route_cue: { name: cue.name, ...(n && n > 0 ? { after_ms: n } : {}) } });
                }}
              />
            </label>
          </div>
          <SettingHelp id="route_cue" />
        </fieldset>
      )}
        </details>
      )}

      {published && (
        <fieldset className="stage-box">
          <legend>Published version</legend>
          <p>
            v{published.number}
            {published.createdAt && <span className="subtext"> · {new Date(published.createdAt).toLocaleString()}</span>}
          </p>
          <p className="subtext">
            Config hash <code>{published.hash}</code>
          </p>
          {onRestore && published.number > 1 &&
            (restoring ? (
              <div role="group" aria-label="Confirm restore" className="restore-confirm">
                <p>
                  Publish v{published.number - 1}'s settings as v{published.number + 1}? Calls already in progress finish on the version they started with. Changes you have not published here are replaced.
                </p>
                <div className="row">
                  <button
                    type="button"
                    className="action-button"
                    onClick={() => {
                      setRestoreError(null);
                      onRestore().catch((e: unknown) => {
                        setRestoring(false);
                        setRestoreError(e instanceof Error ? e.message : "The previous version could not be restored.");
                      });
                    }}
                  >
                    Restore v{published.number - 1}
                  </button>
                  <button type="button" className="action-button-small" onClick={() => setRestoring(false)}>
                    Cancel
                  </button>
                </div>
              </div>
            ) : (
              <button type="button" className="action-button-small" onClick={() => setRestoring(true)}>
                Restore v{published.number - 1}…
              </button>
            ))}
          {restoreError && (
            <p role="alert" className="note note--bad">
              {restoreError}
            </p>
          )}
        </fieldset>
      )}
    </SidePanel>
  );
}
