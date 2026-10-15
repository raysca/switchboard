import { useMemo, useRef, useState } from "react";
import { Link } from "react-router";
import type { FlowDTO, ApiClient, CreateFlowInput, ProviderStatus, ToolSelection } from "../../../api/client";
import { SidePanel } from "../../../components/SidePanel";
import { Status } from "../../../components/primitives";
import { FlowVoiceTest } from "../FlowVoiceTest";
import { Canvas, type Selection } from "./Canvas";
import { describeChanges, describeToolChanges } from "./diff";
import { ToolPicker } from "./ToolPicker";
import { FlowSettingsPanel } from "./FlowSettingsPanel";
import {
  MAX_STEPS,
  addDraftRoute,
  applySelection,
  insertStep,
  emptyFlow,
  flowIssues,
  fromFlowConfig,
  stepIssues,
  toPayload,
  unreachableSteps,
  uniqueName,
  updateStep,
  validate,
  type FlowDraft,
  type StepTab,
} from "./model";
import { PublishDialog } from "./PublishDialog";
import { StepPanel } from "./StepPanel";
import { NO_CALL, useCallView, type CallInfo } from "./useCallView";

type Panel = { kind: "step"; id: string } | { kind: "settings" } | { kind: "call" } | null;

const sortKeys = (_k: string, v: unknown) =>
  v && typeof v === "object" && !Array.isArray(v) ? Object.fromEntries(Object.entries(v).sort(([a], [b]) => (a < b ? -1 : 1))) : v;
// The server stores configs key-sorted; compare that way so key order is not a "change".
const canonical = (v: unknown) => JSON.stringify(v, sortKeys);

const canCall = (a: FlowDTO | null) => !!a && (a.readiness.state === "ready" || a.readiness.state === "ready_with_tool_warning");

export type FlowEditorProps = {
  /** The name a new flow starts with (from the New flow dialog). Ignored for a stored flow. */
  initialName?: string;
  /** Open the Test call panel on arrival, when the flow can take calls. */
  initialPanel?: "call";
  flow: FlowDTO | null;
  providers: ProviderStatus[];
  client: ApiClient;
  keyValue: string | null;
  pending: boolean;
  onSubmit: (input: CreateFlowInput) => void;
  onCancel: () => void;
  /** Calls running on the published version right now (for the Publish dialog). */
  callsInProgress?: number;
};

const toolKey = (t: ToolSelection) => `${t.mcp_server_id}/${t.remote_name}`;
const sameTools = (a: ToolSelection[], b: ToolSelection[]) => {
  const norm = (l: ToolSelection[]) => JSON.stringify([...l].sort((x, y) => toolKey(x).localeCompare(toolKey(y))).map((t) => [toolKey(t), t.schema_hash, t.required]));
  return norm(a) === norm(b);
};

/** One page for a flow: the flow stays put; steps, settings and test calls slide in from the right. */
export function FlowEditor({ flow, providers, client, keyValue, pending, onSubmit, onCancel, initialName, initialPanel, callsInProgress }: FlowEditorProps) {
  const publishedTools = useMemo(() => flow?.current_version?.mcp_tools ?? [], [flow]);
  const published = useMemo(() => (flow?.current_version ? fromFlowConfig(flow.current_version.config, publishedTools.map(toolKey)) : null), [flow, publishedTools]);
  // The flow's MCP tool selection is part of the draft: it is published and tested with the config.
  const [tools, setTools] = useState<ToolSelection[]>(publishedTools);
  const flowTools = useMemo(() => tools.map(toolKey), [tools]);
  const toolsChanged = !sameTools(tools, publishedTools);
  const [name, setName] = useState(flow?.name ?? initialName ?? "");
  const [description, setDescription] = useState(flow?.description ?? "");
  const [graph, setGraph] = useState<FlowDraft>(() => published ?? emptyFlow());
  const [panel, setPanel] = useState<Panel>(initialPanel === "call" && canCall(flow) ? { kind: "call" } : null);
  const [tab, setTab] = useState<StepTab>("basics");
  const [focusRoute, setFocusRoute] = useState<string | undefined>();
  const [freshStep, setFreshStep] = useState<string | null>(null);
  const [wide, setWide] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [call, setCall] = useState<CallInfo>(NO_CALL);
  const opener = useRef<HTMLElement | null>(null);
  // The latest flow, for a panel that closes because its step was deleted (React Flow drops the step's DOM a beat later).
  const graphNow = useRef(graph);
  graphNow.current = graph;

  const version = flow?.current_version?.number ?? 0;
  const renamed = !!flow && name.trim() !== flow.name;
  const redescribed = !!flow && description.trim() !== flow.description;
  const unpublished = !published || renamed || redescribed || toolsChanged || canonical(toPayload(published)) !== canonical(toPayload(graph));
  const needSetup = graph.steps.filter((n) => stepIssues(graph, n.id).length > 0).length;
  const flowProblems = flowIssues(graph);
  const problems = [...(name.trim() ? [] : ["Give the flow a name in Flow settings."]), ...validate(graph)];
  const callable = canCall(flow);
  // A test call runs what is on the canvas; changes that could not be published cannot be tested.
  const draft = unpublished && problems.length === 0 ? { config: toPayload(graph), ...(toolsChanged ? { mcp_tools: tools } : {}) } : undefined;
  // The call runs the published version, or the canvas when it is a draft test, so the live view follows that flow.
  const live = useCallView(draft ? graph : (published ?? graph), call, client);
  const activeStep = graph.steps.find((n) => live.view.steps[n.id]?.state === "active")?.name;

  const state: { tone: "neutral" | "good" | "bad"; text: string } =
    graph.steps.length === 0
      ? { tone: "neutral", text: "No steps yet" }
      : needSetup > 0
        ? { tone: "bad", text: `${needSetup} ${needSetup === 1 ? "step needs" : "steps need"} setup` }
        : flowProblems.length > 0
          ? { tone: "bad", text: flowProblems[0] }
          : { tone: "good", text: unpublished ? "Ready to publish" : "Published" };

  const changeTools = (next: ToolSelection[]) => {
    setTools(next);
    setGraph((g) => applySelection(g, next));
  };

  const startStep = graph.steps.find((n) => n.id === graph.start);
  const asyncKeys = Array.isArray(startStep?.config.async_tools) ? (startStep.config.async_tools as string[]) : [];
  const changeAsync = (keys: string[]) => {
    setGraph((g) => {
      const start = g.steps.find((n) => n.id === g.start);
      if (!start) return g;
      const { async_tools: _drop, ...rest } = start.config;
      return updateStep(g, start.id, { config: keys.length ? { ...rest, async_tools: keys } : rest });
    });
  };

  const open = (next: Panel, from?: HTMLElement | null) => {
    opener.current = from ?? (document.activeElement as HTMLElement | null);
    setPanel(next);
  };
  const close = () => {
    setPanel(null);
    setFocusRoute(undefined);
    setFreshStep(null);
  };
  /** Lands on the first tab with a problem; re-selecting the same step keeps the current tab. */
  const openStep = (g: FlowDraft, id: string, forced?: StepTab) => {
    const issues = stepIssues(g, id);
    const sameStep = panel?.kind === "step" && panel.id === id;
    setTab(forced ?? issues[0]?.tab ?? (sameStep ? tab : "basics"));
    if (!forced) setFocusRoute(undefined);
    setFreshStep(null);
    open({ kind: "step", id });
  };
  const addStep = () => {
    const next = insertStep(graph, uniqueName(graph, "New step"));
    if (next === graph) return;
    const id = next.steps[next.steps.length - 1].id;
    setGraph(next);
    openStep(next, id, "basics");
    setFreshStep(id);
  };
  const drawRoute = (from: string, to: string) => {
    if (call.live) return;
    const next = addDraftRoute(graph, from, to);
    if (next === graph) return;
    setGraph(next);
    setFocusRoute(next.routes[next.routes.length - 1].id);
    openStep(next, from, "routes");
  };
  const select = (s: Selection) => {
    if (call.live) return;
    if (s?.kind === "step") openStep(graph, s.id);
    else if (s?.kind === "route") {
      const e = graph.routes.find((x) => x.id === s.id);
      if (e) {
        setFocusRoute(e.id);
        openStep(graph, e.from, "routes");
      }
    } else if (panel?.kind === "step") close();
  };
  const publish = () => {
    onSubmit({
      name: name.trim(),
      description: description.trim(),
      enabled: flow?.enabled ?? true,
      config: toPayload(graph) as CreateFlowInput["config"],
      ...(toolsChanged ? { mcp_tools: tools } : {}),
    });
    setConfirming(false);
  };

  const stageClass = ["flow-editor__stage", panel ? "flow-editor__stage--panel" : "", panel?.kind === "step" && wide ? "flow-editor__stage--wide" : ""]
    .join(" ")
    .trim();

  return (
    <div className="flow-editor">
      <header className="flow-editor__head">
        <nav aria-label="Breadcrumb" className="flow-editor__crumb">
          <Link to="/flows">Flows</Link> /
        </nav>
        <h1 id="flow-editor-title" tabIndex={-1}>
          {name.trim() || "New flow"}
        </h1>
        <span className="chip">{flow ? `v${version}${unpublished ? " · unpublished changes" : ""}` : "Never published"}</span>
        <Status tone={state.tone}>{state.text}</Status>
        <span className="grow" />
        <button
          type="button"
          className="action-button-small"
          aria-pressed={panel?.kind === "settings"}
          disabled={call.live}
          title={call.live ? "End the test call first" : undefined}
          onClick={(e) => open({ kind: "settings" }, e.currentTarget)}
        >
          Flow settings
        </button>
        <button
          type="button"
          className="action-button-small"
          disabled={!callable}
          title={!flow ? "Publish once to test" : !callable ? "This flow is not ready to take calls" : undefined}
          onClick={(e) => open({ kind: "call" }, e.currentTarget)}
        >
          Test call
        </button>
        <button type="button" className="action-button" disabled={pending} onClick={() => setConfirming(true)}>
          {pending ? "Publishing…" : "Publish"}
        </button>
        <button type="button" className="action-button-small" onClick={onCancel}>
          Close
        </button>
      </header>

      <div className="flow-editor__toolbar">
        {graph.steps.length > 0 && (
          <button
            type="button"
            className="action-button-small flow-editor__add"
            disabled={graph.steps.length >= MAX_STEPS || call.live}
            title={graph.steps.length >= MAX_STEPS ? `A flow has at most ${MAX_STEPS} steps` : undefined}
            onClick={addStep}
          >
            Add step
          </button>
        )}
        <span className="subtext">
          {graph.steps.length} of {MAX_STEPS} steps
        </span>
        <span className="grow" />
        {graph.steps.length > 0 && !call.live && <span className="subtext">Drag from the dot on a step's route to add a route</span>}
      </div>

      <div className={stageClass}>
        {graph.steps.length === 0 ? (
          <div className="flow-editor__blank">
            <div className="flow-editor__blank-card">
              <span className="flow-editor__plus" aria-hidden="true">
                +
              </span>
              <h3>Start with one step.</h3>
              <p>A step answers the caller. Give it instructions, a model and a voice: that's enough to make a test call. Add more steps and routes when you need them.</p>
              <button type="button" className="action-button" onClick={addStep}>
                Add first step
              </button>
            </div>
          </div>
        ) : (
          <Canvas
            graph={graph}
            height="auto"
            fitKey={panel ? `${panel.kind}${wide ? "-wide" : ""}` : "none"}
            selected={panel?.kind === "step" ? { kind: "step", id: panel.id } : null}
            onSelect={select}
            onMove={(id, position) => setGraph((g) => updateStep(g, id, { position }))}
            onConnect={drawRoute}
            visual={call.live ? live.view.steps : undefined}
            traversedRoutes={call.live ? live.traversedRoutes : undefined}
            justHandedRoute={live.justHanded}
          />
        )}

        {panel?.kind === "step" && (
          <StepPanel
            key={panel.id}
            graph={graph}
            stepId={panel.id}
            providers={providers}
            flowTools={flowTools}
            toolPicker={<ToolPicker client={client} selected={tools} onChange={changeTools} asyncKeys={asyncKeys} onAsyncChange={changeAsync} />}
            tab={tab}
            onTab={setTab}
            focusRoute={focusRoute}
            onFocusRoute={setFocusRoute}
            focusName={freshStep === panel.id}
            wide={wide}
            onToggleWide={() => setWide((w) => !w)}
            onChange={setGraph}
            onClose={close}
            returnFocusTo={() => (graphNow.current.steps.some((n) => n.id === panel.id) ? opener.current : null)}
            fallbackFocusId="flow-editor-title"
          />
        )}
        {panel?.kind === "settings" && (
          <FlowSettingsPanel
            published={
              flow?.current_version
                ? { number: flow.current_version.number, createdAt: flow.current_version.created_at, hash: flow.current_version.config_hash }
                : undefined
            }
            graph={graph}
            providers={providers}
            name={name}
            description={description}
            onName={setName}
            onDescription={setDescription}
            onChange={setGraph}
            onClose={close}
            returnFocusTo={opener.current}
            fallbackFocusId="flow-editor-title"
          />
        )}
        {panel?.kind === "call" && flow && (
          <SidePanel title="Test call" tone="dark" onClose={close} returnFocusTo={opener.current} fallbackFocusId="flow-editor-title">
            {draft && (
              <p role="note" className="note">
                This call runs your unpublished changes. They are not saved or published.
              </p>
            )}
            <FlowVoiceTest
              flow={flow}
              client={client}
              keyValue={keyValue}
              onCall={setCall}
              activeStep={activeStep}
              draft={draft}
              blocked={unpublished && problems.length > 0 ? `Fix this before you can test your changes: ${problems[0]}` : undefined}
            />
          </SidePanel>
        )}
      </div>

      {confirming && (
        <PublishDialog
          flowName={name}
          version={version + 1}
          currentVersion={flow ? version : undefined}
          callsInProgress={callsInProgress}
          unreachable={unreachableSteps(graph)}
          firstVersion={!published}
          changes={[
            ...(renamed ? [`Renamed the flow to ${name.trim()}`] : []),
            ...(redescribed ? ["Description changed"] : []),
            ...describeChanges(published, graph),
            ...describeToolChanges(publishedTools, tools),
          ]}
          problems={problems}
          pending={pending}
          onConfirm={publish}
          onCancel={() => setConfirming(false)}
        />
      )}
    </div>
  );
}
