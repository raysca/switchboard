import { useState } from "react";
import type { ProviderStatus } from "../../../api/client";
import { SidePanel } from "../../../components/SidePanel";
import { StageFields, type StageValue } from "../StageFields";
import { StepAdvanced } from "./StepAdvanced";
import {
  MAX_STEPS,
  addDraftRoute,
  insertStep,
  removeRoute,
  removeStep,
  setStart,
  stepIssues,
  uniqueName,
  updateRoute,
  updateStep,
  type FlowDraft,
  type StepTab,
} from "./model";

const TABS: { id: StepTab; label: string }[] = [
  { id: "basics", label: "Basics" },
  { id: "routes", label: "Routes" },
  { id: "tools", label: "Tools" },
  { id: "advanced", label: "Advanced" },
];
const rec = (v: unknown): Record<string, unknown> =>
  v && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : {};
const str = (v: unknown): string => (typeof v === "string" ? v : "");

export type StepPanelProps = {
  graph: FlowDraft;
  stepId: string;
  providers: ProviderStatus[];
  /** MCP tools selected on the flow ("<mcp_server_id>/<remote_name>"); a step may call a subset. */
  flowTools: string[];
  tab: StepTab;
  onTab: (tab: StepTab) => void;
  /** The route whose condition takes focus: one just drawn or added. */
  focusRoute?: string;
  onFocusRoute: (routeId: string | undefined) => void;
  /** A step just added: its name takes focus, selected, so typing renames it. */
  focusName?: boolean;
  wide: boolean;
  onToggleWide: () => void;
  onChange: (next: FlowDraft) => void;
  onClose: () => void;
  returnFocusTo?: HTMLElement | null | (() => HTMLElement | null);
  fallbackFocusId?: string;
};

/** Everything about one step, in tabs; Basics alone makes a step ready. */
export function StepPanel(props: StepPanelProps) {
  const { graph, stepId, providers, flowTools, tab, onTab, focusRoute, onFocusRoute, focusName, wide, onToggleWide, onChange, onClose, returnFocusTo, fallbackFocusId } = props;
  const [routeTarget, setRouteTarget] = useState("");
  const step = graph.steps.find((n) => n.id === stepId);
  if (!step) return null;

  const isStart = step.id === graph.start;
  const issues = stepIssues(graph, step.id);
  const routes = graph.routes.filter((e) => e.from === step.id);
  const targets = graph.steps.filter((n) => n.id !== step.id && !routes.some((r) => r.to === n.id));
  const nameOf = (id: string) => graph.steps.find((n) => n.id === id)?.name.trim() || "Untitled step";
  const llm = rec(step.config.llm);
  const tts = rec(step.config.tts);
  const patchStage = (cap: "llm" | "tts", patch: Record<string, unknown>) =>
    onChange(updateStep(graph, step.id, { config: { ...step.config, [cap]: { ...rec(step.config[cap]), ...patch } } }));
  const warn = (t: StepTab) => issues.some((i) => i.tab === t);
  const count = (t: StepTab) => (t === "routes" ? routes.length : t === "tools" ? step.tools.length + (step.end_call ? 1 : 0) : null);
  const basicsIssues = issues.filter((i) => i.tab === "basics");

  const focusLastRoute = (next: FlowDraft) => {
    onChange(next);
    onFocusRoute(next.routes[next.routes.length - 1].id);
  };
  const addRoute = () => {
    const next = addDraftRoute(graph, step.id, routeTarget);
    if (next === graph) return;
    focusLastRoute(next);
    setRouteTarget("");
  };
  const routeToNewStep = () => {
    const withStep = insertStep(graph, uniqueName(graph, "New step"));
    if (withStep === graph) return;
    focusLastRoute(addDraftRoute(withStep, step.id, withStep.steps[withStep.steps.length - 1].id));
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
                  onChange={(v: StageValue) => patchStage("tts", { provider: v.provider, voice: v.voice ?? "" })}
                />
              </fieldset>
            </div>
          </>
        )}

        {tab === "routes" && (
          <>
            <p className="subtext">
              When the model decides a condition is true, the caller moves to that step. You can also drag from a step’s right route on the graph.
            </p>
            {routes.length === 0 && <p className="subtext">No routes. The call stays on this step until it ends.</p>}
            <ul className="plain-list route-list">
              {routes.map((r) => (
                <li key={r.id} className="route-list__item">
                  <label className="form-field" htmlFor={`route-${r.id}`}>
                    <span>Condition for the route to {nameOf(r.to)}</span>
                    <textarea
                      id={`route-${r.id}`}
                      rows={2}
                      maxLength={512}
                      value={r.condition}
                      autoFocus={r.id === focusRoute}
                      aria-invalid={!r.condition.trim()}
                      placeholder="For example: the caller asks about an invoice"
                      onChange={(e) => onChange(updateRoute(graph, r.id, e.target.value))}
                      onBlur={() => r.id === focusRoute && onFocusRoute(undefined)}
                    />
                  </label>
                  <button type="button" className="action-button-small action-button-danger" onClick={() => onChange(removeRoute(graph, r.id))}>
                    Remove the route to {nameOf(r.to)}
                  </button>
                </li>
              ))}
            </ul>
            <div className="row row--end">
              <label className="form-field grow" htmlFor="route-target">
                <span>Route to</span>
                <select id="route-target" value={routeTarget} onChange={(e) => setRouteTarget(e.target.value)}>
                  <option value="">Choose a step</option>
                  {targets.map((n) => (
                    <option key={n.id} value={n.id}>
                      {n.name.trim() || "Untitled step"}
                    </option>
                  ))}
                </select>
              </label>
              <button type="button" className="action-button-small" disabled={!routeTarget} onClick={addRoute}>
                Add route
              </button>
            </div>
            <button type="button" className="action-button-small" disabled={graph.steps.length >= MAX_STEPS} onClick={routeToNewStep}>
              Route to a new step
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
                  disabled={graph.steps.length === 1}
                  onChange={(e) => onChange(updateStep(graph, step.id, { end_call: e.target.checked }))}
                />
                Can end the call
              </label>
              <p className="subtext">Gives this step an end_call tool. The model uses it after saying goodbye; the call ends when it stops speaking. {graph.steps.length === 1 ? " Add a second step first: a one-step flow needs a second step to end the call." : " Needs a flow with at least two steps."}</p>
            </fieldset>
            {flowTools.length === 0 ? (
              <p className="subtext">This flow has no tools. Choose MCP tools for the flow first.</p>
            ) : (
              <fieldset className="stage-box">
                <legend>Tools this step can call</legend>
                {flowTools.map((t) => (
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
                {graph.steps.length === 1 && <p className="subtext">With one step, it can call every tool the flow has.</p>}
              </fieldset>
            )}
          </>
        )}

        {tab === "advanced" && <StepAdvanced graph={graph} step={step} providers={providers} onChange={onChange} />}
      </div>
    </SidePanel>
  );
}
