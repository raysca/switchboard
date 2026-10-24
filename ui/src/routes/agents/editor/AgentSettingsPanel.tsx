import { useState } from "react";
import type { ProviderStatus, ServerConfigDTO } from "../../../api/client";
import { VersionHistory, type VersionHistoryProps } from "./VersionHistory";
import { SidePanel } from "../../../components/SidePanel";
import { StageFields, type StageValue } from "../StageFields";
import { AudioFields, QuietCallerBasics, QuietCallerDetails } from "./CallBehaviorFields";
import { EndOfTurnFields, TurnFields } from "./TurnFields";
import { SettingHelp } from "./SettingHelp";
import { CALL_LIMIT_RANGE_S, DEFAULT_FAILOVER, FAILOVER_CLASSES, agentIssues, MAX_SUMMARY_PROMPT_BYTES, STT_P99_RANGE, updateStep, type AgentDraft, type StepConfig } from "./model";

type Rec = Record<string, unknown>;
const rec = (v: unknown): Rec => (v && typeof v === "object" && !Array.isArray(v) ? (v as Rec) : {});
const str = (v: unknown): string => (typeof v === "string" ? v : "");
const without = (o: Rec, key: string): Rec => {
  const { [key]: _drop, ...rest } = o;
  return rest;
};
/** An empty number field removes the key: the server default applies. */
const numberOrNothing = (raw: string): number | null => (raw.trim() === "" ? null : Math.round(Number(raw)));

export type AgentSettingsPanelProps = {
  /** The published version, shown read-only: what is live, and its content hash. */
  published?: { number: number; createdAt: string; hash: string };
  /** Read the version history and publish an earlier version again. Absent for an agent that is not saved yet. */
  history?: VersionHistoryProps;
  graph: AgentDraft;
  providers: ProviderStatus[];
  /** What the server offers and defaults to; absent while it loads or if it cannot be read. */
  serverConfig?: ServerConfigDTO;
  name: string;
  description: string;
  onName: (v: string) => void;
  onDescription: (v: string) => void;
  onChange: (g: AgentDraft) => void;
  onClose: () => void;
  returnFocusTo?: HTMLElement | null;
  fallbackFocusId?: string;
};

/** What the whole call shares. Stored on the start step (server: call-level config). */
export function AgentSettingsPanel({ graph, providers, serverConfig, name, description, onName, onDescription, onChange, onClose, returnFocusTo, fallbackFocusId, published, history }: AgentSettingsPanelProps) {
  const start = graph.steps.find((n) => n.id === graph.start);
  const cfg: StepConfig = start?.config ?? {};
  const stt = rec(cfg.stt);
  const context = rec(cfg.context);
  const recording = rec(cfg.recording);
  const tts = rec(cfg.tts);
  const problems = agentIssues(graph, serverConfig && { maxMuteMs: serverConfig.turn.max_mute_ms, maxCallSeconds: serverConfig.max_call_duration_seconds });
  const advancedSet = (["vad_stop_ms", "stt_p99_ms", "context", "audio", "turn", "failover", "max_call_duration_seconds"] as const).filter((k) => k in cfg).length +
    (tts.clean_text === false ? 1 : 0) +
    (Object.keys(rec(cfg.idle)).some((k) => k !== "user_timeout_ms") ? 1 : 0) +
    (graph.handoff_cue && graph.handoffs.length > 0 ? 1 : 0);
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
  const cue = graph.handoff_cue;
  const cueNames = serverConfig?.audio.cues ?? [];
  const cueUnknown = cue !== undefined && cue.name !== "" && !cueNames.includes(cue.name);
  const limitMaxS = serverConfig ? Math.min(CALL_LIMIT_RANGE_S[1], serverConfig.max_call_duration_seconds) : CALL_LIMIT_RANGE_S[1];
  const limitPossible = limitMaxS >= CALL_LIMIT_RANGE_S[0];
  const failoverOn = Array.isArray(rec(cfg.failover).on) ? (rec(cfg.failover).on as string[]) : [];
  const failover = failoverOn.length ? failoverOn : DEFAULT_FAILOVER;
  const setFailover = (id: string, on: boolean) => {
    const next = FAILOVER_CLASSES.map((c): string => c.id).filter((c) => (c === id ? on : failover.includes(c)));
    const isDefault = next.length === DEFAULT_FAILOVER.length && DEFAULT_FAILOVER.every((c) => next.includes(c));
    setKey("failover", isDefault ? null : { on: next });
  };

  return (
    <SidePanel
      title="Agent settings"
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
      <label className="form-field" htmlFor="agent-name">
        <span>Agent name</span>
        <input id="agent-name" type="text" maxLength={128} value={name} onChange={(e) => onName(e.target.value)} />
      </label>
      <label className="form-field" htmlFor="agent-description">
        <span>Description</span>
        <textarea id="agent-description" rows={2} maxLength={1024} value={description} onChange={(e) => onDescription(e.target.value)} />
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
              idPrefix="agent-stt"
              value={{ provider: str(stt.provider), model: str(stt.model), language: str(stt.language) }}
              onChange={(v: StageValue) => setKey("stt", { ...stt, provider: v.provider, model: v.model ?? "", language: v.language ?? "" })}
            />
          </fieldset>
          <fieldset className="stage-box">
            <legend>Recording</legend>
            <label className="check-row" htmlFor="agent-record">
              <input
                id="agent-record"
                type="checkbox"
                checked={recording.enabled === true}
                onChange={(e) => setKey("recording", e.target.checked ? { enabled: true, disclosure_confirmed: recording.disclosure_confirmed === true } : null)}
              />
              <span>Record calls</span>
            </label>
            <label className="check-row" htmlFor="agent-disclosure">
              <input
                id="agent-disclosure"
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
            <legend>Speaking</legend>
            <label className="check-row" htmlFor="agent-clean-text">
              <input
                id="agent-clean-text"
                type="checkbox"
                checked={tts.clean_text !== false}
                onChange={(e) => setKey("tts", e.target.checked ? without(tts, "clean_text") : { ...tts, clean_text: false })}
              />
              <span>Clean text before speaking (strip markdown, links and emoji)</span>
            </label>
            <SettingHelp id="tts.clean_text" />
          </fieldset>
          <fieldset className="stage-box">
            <legend>When to move to a fallback</legend>
            <p className="subtext">Only matters when a stage has fallbacks.</p>
            {FAILOVER_CLASSES.map((c) => (
              <label key={c.id} className="check-row" htmlFor={`agent-failover-${c.id}`}>
                <input
                  id={`agent-failover-${c.id}`}
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
            <legend>Turn-taking</legend>
            <label className="form-field" htmlFor="agent-vad">
              <span>Wait after the caller stops (ms)</span>
              <input
                id="agent-vad"
                type="number"
                min={200}
                max={2000}
                placeholder="Server default"
                aria-describedby="agent-vad-hint"
                value={typeof cfg.vad_stop_ms === "number" ? cfg.vad_stop_ms : ""}
                onChange={(e) => setKey("vad_stop_ms", numberOrNothing(e.target.value))}
              />
            </label>
            <p id="agent-vad-hint" className="subtext">
              200 to 2000. Shorter replies sooner; longer interrupts the caller less.
            </p>
            <SettingHelp id="vad_stop_ms" />
            <label className="form-field" htmlFor="agent-stt-p99">
              <span>Wait for a late transcript (ms)</span>
              <input
                id="agent-stt-p99"
                type="number"
                min={STT_P99_RANGE[0]}
                max={STT_P99_RANGE[1]}
                placeholder="Server default"
                aria-describedby="agent-stt-p99-hint"
                value={typeof cfg.stt_p99_ms === "number" ? cfg.stt_p99_ms : ""}
                onChange={(e) => setKey("stt_p99_ms", numberOrNothing(e.target.value))}
              />
            </label>
            <p id="agent-stt-p99-hint" className="subtext">
              {STT_P99_RANGE[0]} to {STT_P99_RANGE[1]}. Empty: the server default.
            </p>
            <SettingHelp id="stt_p99_ms" />
            <EndOfTurnFields cfg={cfg} setKey={setKey} serverConfig={serverConfig} />
          </fieldset>
          <TurnFields cfg={cfg} setKey={setKey} serverConfig={serverConfig} />
          <fieldset className="stage-box">
            <legend>Call length</legend>
            <label className="form-field" htmlFor="agent-call-limit">
              <span>Longest call (minutes)</span>
              <input
                id="agent-call-limit"
                type="number"
                min={CALL_LIMIT_RANGE_S[0] / 60}
                max={limitMaxS / 60}
                step="any"
                disabled={!limitPossible}
                placeholder={serverConfig ? String(serverConfig.max_call_duration_seconds / 60) : "Server limit"}
                aria-describedby="agent-call-limit-hint"
                value={typeof cfg.max_call_duration_seconds === "number" ? cfg.max_call_duration_seconds / 60 : ""}
                onChange={(e) => {
                  const n = e.target.value.trim() === "" ? NaN : Number(e.target.value);
                  setKey("max_call_duration_seconds", Number.isFinite(n) ? Math.round(n * 60) : null);
                }}
              />
            </label>
            <p id="agent-call-limit-hint" className="subtext">
              {limitPossible
                ? `${CALL_LIMIT_RANGE_S[0] / 60} to ${limitMaxS / 60}. Empty: the server's limit.`
                : `The server allows at most ${limitMaxS} seconds, so no per-agent limit can be set.`}
            </p>
            {typeof cfg.max_call_duration_seconds === "number" && cfg.max_call_duration_seconds <= 0 && (
              <p role="alert" className="note note--bad">
                Longest call must be {CALL_LIMIT_RANGE_S[0] / 60} to {CALL_LIMIT_RANGE_S[1] / 60} minutes. Clear the field to use the server's limit.
              </p>
            )}
            <SettingHelp id="max_call_duration_seconds" />
          </fieldset>
          <fieldset className="stage-box">
            <legend>Memory</legend>
            <div className="grid-2">
              <label className="form-field" htmlFor="agent-max-messages">
                <span>Max messages</span>
                <input
                  id="agent-max-messages"
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
              <label className="form-field" htmlFor="agent-max-tokens">
                <span>Max tokens</span>
                <input
                  id="agent-max-tokens"
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
            <label className="check-row" htmlFor="agent-summarise">
              <input
                id="agent-summarise"
                type="checkbox"
                checked={rec(context.summarization).enabled === true}
                onChange={(e) => setContext(e.target.checked ? { ...context, summarization: { enabled: true } } : without(context, "summarization"))}
              />
              <span>Summarise older turns</span>
            </label>
            <SettingHelp id="summarization.enabled" />
            {summary.enabled === true && (
              <>
                <label className="form-field" htmlFor="agent-summary-trigger">
                  <span>Summarise when history reaches (tokens)</span>
                  <input
                    id="agent-summary-trigger"
                    type="number"
                    min={1}
                    placeholder="Three quarters of Max tokens"
                    aria-describedby="agent-summary-trigger-hint"
                    value={typeof summary.trigger_tokens === "number" ? summary.trigger_tokens : ""}
                    onChange={(e) => setSummary("trigger_tokens", numberOrNothing(e.target.value))}
                  />
                </label>
                <p id="agent-summary-trigger-hint" className="subtext">
                  Empty: three quarters of Max tokens. Must be below Max tokens.
                </p>
                <SettingHelp id="summarization.trigger_tokens" />
                <label className="form-field" htmlFor="agent-summary-keep">
                  <span>Keep the latest turns</span>
                  <input
                    id="agent-summary-keep"
                    type="number"
                    min={1}
                    placeholder="4"
                    aria-describedby="agent-summary-keep-hint"
                    value={typeof summary.keep_turns === "number" ? summary.keep_turns : ""}
                    onChange={(e) => setSummary("keep_turns", numberOrNothing(e.target.value))}
                  />
                </label>
                <p id="agent-summary-keep-hint" className="subtext">
                  Empty: 4.
                </p>
                <SettingHelp id="summarization.keep_turns" />
                <label className="form-field" htmlFor="agent-summary-prompt">
                  <span>Summary instructions</span>
                  <textarea
                    id="agent-summary-prompt"
                    rows={3}
                    placeholder="Built-in instructions"
                    aria-describedby="agent-summary-prompt-hint"
                    value={typeof summary.prompt === "string" ? summary.prompt : ""}
                    onChange={(e) => setSummary("prompt", e.target.value === "" ? null : e.target.value)}
                  />
                </label>
                <p id="agent-summary-prompt-hint" className="subtext">
                  Empty: the built-in prompt. At most {MAX_SUMMARY_PROMPT_BYTES} bytes.
                </p>
                <SettingHelp id="summarization.prompt" />
              </>
            )}
          </fieldset>
      {graph.handoffs.length > 0 && (
        <fieldset className="stage-box">
          <legend>Handoff sound</legend>
          <p className="subtext">Plays while the caller moves between steps. The sounds are the ones set up on the server (audio.cues).</p>
          <div className="grid-2">
            <label className="form-field" htmlFor="cue-name">
              <span>Handoff sound</span>
              <select
                id="cue-name"
                value={cue?.name ?? ""}
                onChange={(e) => {
                  const v = e.target.value;
                  const { handoff_cue: _drop, ...rest } = graph;
                  onChange(v ? { ...rest, handoff_cue: { ...(cue ?? {}), name: v } } : rest);
                }}
              >
                <option value="">None</option>
                {cueUnknown && <option value={cue.name}>{cue.name} (not on this server)</option>}
                {cueNames.map((n) => (
                  <option key={n} value={n}>
                    {n}
                  </option>
                ))}
              </select>
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
                  onChange({ ...graph, handoff_cue: { name: cue.name, ...(n && n > 0 ? { after_ms: n } : {}) } });
                }}
              />
            </label>
          </div>
          <SettingHelp id="handoff_cue" />
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
          {history && <VersionHistory {...history} />}
        </fieldset>
      )}
    </SidePanel>
  );
}
