import { useState, type ReactNode } from "react";
import type { ProviderStatus, ServerConfigDTO } from "../../../api/client";
import { SidePanel } from "../../../components/SidePanel";
import { StageFields, type StageValue } from "../StageFields";
import { StepAdvanced } from "./StepAdvanced";
import {
  MAX_STEPS,
  addDraftHandoff,
  insertStep,
  removeHandoff,
  removeStep,
  setStart,
  stepIssues,
  uniqueName,
  updateHandoff,
  updateStep,
  type AgentDraft,
  type StepTab,
  MAX_GREETING_BYTES,
  byteLength,
} from "./model";

const TABS: { id: StepTab; label: string }[] = [
  { id: "basics", label: "Basics" },
  { id: "handoffs", label: "Handoffs" },
  { id: "tools", label: "Tools" },
  { id: "advanced", label: "Advanced" },
];
const rec = (v: unknown): Record<string, unknown> =>
  v && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : {};
const str = (v: unknown): string => (typeof v === "string" ? v : "");

export type StepPanelProps = {
  graph: AgentDraft;
  stepId: string;
  providers: ProviderStatus[];
  /** What the server offers (its background sounds); absent while it loads. */
  serverConfig?: ServerConfigDTO;
  /** MCP tools selected on the agent ("<mcp_server_id>/<remote_name>"); a step may call a subset. */
  agentTools: string[];
  /** Chooses which MCP tools the agent has: shown above the step's own choice in the Tools tab. */
  toolPicker?: ReactNode;
  tab: StepTab;
  onTab: (tab: StepTab) => void;
  /** The handoff whose condition takes focus: one just drawn or added. */
  focusHandoff?: string;
  onFocusHandoff: (handoffId: string | undefined) => void;
  /** A step just added: its name takes focus, selected, so typing renames it. */
  focusName?: boolean;
  wide: boolean;
  onToggleWide: () => void;
  onChange: (next: AgentDraft) => void;
  onClose: () => void;
  returnFocusTo?: HTMLElement | null | (() => HTMLElement | null);
  fallbackFocusId?: string;
};

/** Everything about one step, in tabs; Basics alone makes a step ready. */
export function StepPanel(props: StepPanelProps) {
  const { graph, stepId, providers, serverConfig, agentTools, toolPicker, tab, onTab, focusHandoff, onFocusHandoff, focusName, wide, onToggleWide, onChange, onClose, returnFocusTo, fallbackFocusId } = props;
  const [handoffTarget, setHandoffTarget] = useState("");
  const step = graph.steps.find((n) => n.id === stepId);
  if (!step) return null;

  const isStart = step.id === graph.start;
  const issues = stepIssues(graph, step.id);
  const handoffs = graph.handoffs.filter((e) => e.from === step.id);
  const targets = graph.steps.filter((n) => n.id !== step.id && !handoffs.some((r) => r.to === n.id));
  const nameOf = (id: string) => graph.steps.find((n) => n.id === id)?.name.trim() || "Untitled step";
  const llm = rec(step.config.llm);
  const tts = rec(step.config.tts);
  const stt = rec(step.config.stt);
  const greetingBytes = byteLength(str(step.config.greeting).trim());
  const patchStage = (cap: "llm" | "tts" | "stt", patch: Record<string, unknown>) =>
    onChange(updateStep(graph, step.id, { config: { ...step.config, [cap]: { ...rec(step.config[cap]), ...patch } } }));
  const warn = (t: StepTab) => issues.some((i) => i.tab === t);
  const count = (t: StepTab) => (t === "handoffs" ? handoffs.length : t === "tools" ? step.tools.length + (step.end_call && graph.steps.length > 1 ? 1 : 0) : null);
  const basicsIssues = issues.filter((i) => i.tab === "basics");

  const focusLastHandoff = (next: AgentDraft) => {
    onChange(next);
    onFocusHandoff(next.handoffs[next.handoffs.length - 1].id);
  };
  const addHandoff = () => {
    const next = addDraftHandoff(graph, step.id, handoffTarget);
    if (next === graph) return;
    focusLastHandoff(next);
    setHandoffTarget("");
  };
  const handoffToNewStep = () => {
    const withStep = insertStep(graph, uniqueName(graph, "New step"));
    if (withStep === graph) return;
    focusLastHandoff(addDraftHandoff(withStep, step.id, withStep.steps[withStep.steps.length - 1].id));
  };

  return (
    <SidePanel
      title={step.name.trim() || "Untitled step"}
      onClose={onClose}
      wide={wide}
      onToggleWide={onToggleWide}
      returnFocusTo={returnFocusTo}
      fallbackFocusId={fallbackFocusId}
      footer={
        <>
          {!isStart && (
            <>
              <button
                type="button"
                className="action-button-small action-button-danger"
                onClick={() => {
                  onChange(removeStep(graph, step.id));
                  onClose();
                }}
              >
                Delete step
              </button>
              <button type="button" className="action-button-small" onClick={() => onChange(setStart(graph, step.id))}>
                Make start step
              </button>
            </>
          )}
          <span className="grow" />
          <button type="button" className="action-button" onClick={onClose}>
            Done
          </button>
        </>
      }
    >
      <div role="tablist" aria-label="Step settings" className="tabs">
        {TABS.map((t) => (
          <button
            key={t.id}
            type="button"
            role="tab"
            id={`step-tab-${t.id}`}
            aria-selected={tab === t.id}
            aria-controls="step-tabpanel"
            className={tab === t.id ? "tab tab--current" : "tab"}
            onClick={() => onTab(t.id)}
          >
            {t.label}
            {count(t.id) !== null && <span className="tab__count">{count(t.id)}</span>}
            {warn(t.id) && <span className="tab__warn" role="img" aria-label="needs attention" />}
          </button>
        ))}
      </div>

      <div role="tabpanel" id="step-tabpanel" aria-labelledby={`step-tab-${tab}`} className="step-panel__tab">
        {tab === "basics" && (
          <>
            {basicsIssues.length > 0 && (
              <div role="note" className="note note--bad">
                <p>Before this step can take calls:</p>
                <ul className="plain-list">
                  {basicsIssues.map((i) => (
                    <li key={i.message}>{i.message}</li>
                  ))}
                </ul>
              </div>
            )}
            <label className="form-field" htmlFor="step-name">
              <span>Step name</span>
              <input
                id="step-name"
                type="text"
                value={step.name}
                maxLength={64}
                autoFocus={focusName}
                onFocus={(e) => focusName && e.currentTarget.select()}
                onChange={(e) => onChange(updateStep(graph, step.id, { name: e.target.value }))}
              />
            </label>
            <label className="form-field" htmlFor="step-instructions">
              <span>Instructions</span>
              <textarea
                id="step-instructions"
                rows={wide ? 12 : 6}
                value={str(llm.system_prompt)}
                placeholder="What should this step do? For example: greet the caller and find out why they are calling."
                aria-describedby="step-instructions-hint"
                onChange={(e) => patchStage("llm", { system_prompt: e.target.value })}
              />
            </label>
            <p id="step-instructions-hint" className="subtext">
              The model only sees this while the call is on this step.
            </p>
            <label className="form-field" htmlFor="step-greeting">
              <span>Greeting</span>
              <textarea
                id="step-greeting"
                rows={3}
                value={str(step.config.greeting)}
                aria-describedby="step-greeting-hint"
                onChange={(e) => {
                  const { greeting: _drop, ...rest } = step.config;
                  void _drop;
                  onChange(updateStep(graph, step.id, { config: e.target.value === "" ? rest : { ...rest, greeting: e.target.value } }));
                }}
              />
            </label>
            <p id="step-greeting-hint" className="subtext">
              {isStart
                ? "Said word for word as soon as the call connects. Leave empty to let the caller speak first."
                : "Said word for word when a call is transferred here, in this step's voice. Leave empty to let the model greet the caller."}
            </p>
            {greetingBytes >= 450 && (
              <p className="subtext" aria-live="polite">
                {greetingBytes} of {MAX_GREETING_BYTES} bytes
              </p>
            )}
            <div className={wide ? "grid-2" : "step-panel__stack"}>
              <fieldset className="stage-box">
                <legend>Model</legend>
                <StageFields
                  kind="llm"
                  providers={providers}
                  idPrefix={`step-${step.id}-llm`}
                  value={{ provider: str(llm.provider), model: str(llm.model) }}
                  onChange={(v: StageValue) => patchStage("llm", { provider: v.provider, model: v.model ?? "" })}
                />
              </fieldset>
              <fieldset className="stage-box">
                <legend>Voice</legend>
                <StageFields
                  kind="tts"
                  providers={providers}
                  idPrefix={`step-${step.id}-tts`}
                  value={{ provider: str(tts.provider), voice: str(tts.voice) }}
                  onChange={(v: StageValue) =>
                    // voice_settings belong to ElevenLabs; the server rejects them on any other provider.
                    patchStage("tts", { provider: v.provider, voice: v.voice ?? "", ...(v.provider === "elevenlabs" ? {} : { voice_settings: undefined }) })
                  }
                />
              </fieldset>
            </div>
            {isStart ? (
              <fieldset className="stage-box">
                <legend>Listening</legend>
                <p className="subtext">Speech-to-text for the whole call: the start step decides it for every step.</p>
                <StageFields
                  kind="stt"
                  providers={providers}
                  idPrefix={`step-${step.id}-stt`}
                  value={{ provider: str(stt.provider), model: str(stt.model), language: str(stt.language) }}
                  onChange={(v: StageValue) => patchStage("stt", { provider: v.provider, model: v.model ?? "", language: v.language ?? "" })}
                />
              </fieldset>
            ) : (
              <p className="subtext">Listening is set once for the whole call, on the start step ({nameOf(graph.start)}).</p>
            )}
          </>
        )}

        {tab === "handoffs" && (
          <>
            <p className="subtext">
              When the model decides a condition is true, the caller moves to that step. You can also drag from a step’s right handoff on the graph.
            </p>
            {handoffs.length === 0 && <p className="subtext">No handoffs. The call stays on this step until it ends.</p>}
            <ul className="plain-list handoff-list">
              {handoffs.map((r) => (
                <li key={r.id} className="handoff-list__item">
                  <label className="form-field" htmlFor={`handoff-${r.id}`}>
                    <span>Condition for the handoff to {nameOf(r.to)}</span>
                    <textarea
                      id={`handoff-${r.id}`}
                      rows={2}
                      maxLength={512}
                      value={r.condition}
                      autoFocus={r.id === focusHandoff}
                      aria-invalid={!r.condition.trim()}
                      placeholder="For example: the caller asks about an invoice"
                      onChange={(e) => onChange(updateHandoff(graph, r.id, e.target.value))}
                      onBlur={() => r.id === focusHandoff && onFocusHandoff(undefined)}
                    />
                  </label>
                  <button type="button" className="action-button-small action-button-danger" onClick={() => onChange(removeHandoff(graph, r.id))}>
                    Remove the handoff to {nameOf(r.to)}
                  </button>
                </li>
              ))}
            </ul>
            <div className="row row--end">
              <label className="form-field grow" htmlFor="handoff-target">
                <span>Hand off to</span>
                <select id="handoff-target" value={handoffTarget} onChange={(e) => setHandoffTarget(e.target.value)}>
                  <option value="">Choose a step</option>
                  {targets.map((n) => (
                    <option key={n.id} value={n.id}>
                      {n.name.trim() || "Untitled step"}
                    </option>
                  ))}
                </select>
              </label>
              <button type="button" className="action-button-small" disabled={!handoffTarget} onClick={addHandoff}>
                Add handoff
              </button>
            </div>
            <button type="button" className="action-button-small" disabled={graph.steps.length >= MAX_STEPS} onClick={handoffToNewStep}>
              Hand off to a new step
            </button>
          </>
        )}

        {tab === "tools" && (
          <>
            <fieldset className="stage-box">
              <legend>Call control</legend>
              <label className="check-row" htmlFor="step-end-call">
                <input
                  id="step-end-call"
                  type="checkbox"
                  checked={!!step.end_call}
                  onChange={(e) => onChange(updateStep(graph, step.id, { end_call: e.target.checked }))}
                />
                Can end the call
              </label>
              <p className="subtext">
                Gives this step an end_call tool. The model uses it after saying goodbye; the call ends when it stops speaking.
              </p>
            </fieldset>
            {toolPicker && (
              <fieldset className="stage-box">
                <legend>Agent tools</legend>
                <p className="subtext">Shared by every step. Each step chooses which of these it can call.</p>
                {toolPicker}
              </fieldset>
            )}
            {agentTools.length === 0 ? (
              <p className="subtext">No tools are chosen for this agent yet.</p>
            ) : graph.steps.length === 1 ? (
              <p className="subtext">A one-step agent can call every tool of the agent.</p>
            ) : (
              <fieldset className="stage-box">
                <legend>Tools this step can call</legend>
                {agentTools.map((t) => (
                  <label key={t} className="check-row" htmlFor={`step-tool-${t}`}>
                    <input
                      id={`step-tool-${t}`}
                      type="checkbox"
                      checked={step.tools.includes(t)}
                      onChange={(e) =>
                        onChange(updateStep(graph, step.id, { tools: e.target.checked ? [...step.tools, t] : step.tools.filter((x) => x !== t) }))
                      }
                    />
                    <code>{t}</code>
                  </label>
                ))}
                {graph.steps.length === 1 && <p className="subtext">With one step, it can call every tool the agent has.</p>}
              </fieldset>
            )}
          </>
        )}

        {tab === "advanced" && <StepAdvanced graph={graph} step={step} providers={providers} serverConfig={serverConfig} onChange={onChange} />}
      </div>
    </SidePanel>
  );
}
