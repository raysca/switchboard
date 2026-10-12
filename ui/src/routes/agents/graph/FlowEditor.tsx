import { useMemo, useRef, useState } from "react";
import { Link } from "react-router";
import type { AgentDTO, ApiClient, CreateAgentInput, ProviderStatus } from "../../../api/client";
import { SidePanel } from "../../../components/SidePanel";
import { Status } from "../../../components/primitives";
import { AgentVoiceTest } from "../AgentVoiceTest";
import { Canvas, type Selection } from "./Canvas";
import { describeChanges } from "./diff";
import { FlowSettingsPanel } from "./FlowSettingsPanel";
import {
  MAX_NODES,
  addDraftRoute,
  addNode,
  emptyFlow,
  flowIssues,
  fromAgentConfig,
  stepIssues,
  toPayload,
  uniqueName,
  updateNode,
  validate,
  type GraphDraft,
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

const canCall = (a: AgentDTO | null) => !!a && (a.readiness.state === "ready" || a.readiness.state === "ready_with_tool_warning");

export type FlowEditorProps = {
  /** The name a new flow starts with (from the New flow dialog). Ignored for a stored flow. */
  initialName?: string;
  /** Open the Test call panel on arrival, when the flow can take calls. */
  initialPanel?: "call";
  agent: AgentDTO | null;
  providers: ProviderStatus[];
  client: ApiClient;
  keyValue: string | null;
  pending: boolean;
  onSubmit: (input: CreateAgentInput) => void;
  onCancel: () => void;
};

/** One page for a flow: the graph stays put; steps, settings and test calls slide in from the right. */
export function FlowEditor({ agent, providers, client, keyValue, pending, onSubmit, onCancel, initialName, initialPanel }: FlowEditorProps) {
  const flowTools = useMemo(() => (agent?.current_version?.mcp_tools ?? []).map((t) => `${t.mcp_server_id}/${t.remote_name}`), [agent]);
  const published = useMemo(() => (agent?.current_version ? fromAgentConfig(agent.current_version.config, flowTools) : null), [agent, flowTools]);
  const [name, setName] = useState(agent?.name ?? initialName ?? "");
  const [description, setDescription] = useState(agent?.description ?? "");
  const [graph, setGraph] = useState<GraphDraft>(() => published ?? emptyFlow());
  const [panel, setPanel] = useState<Panel>(initialPanel === "call" && canCall(agent) ? { kind: "call" } : null);
  const [tab, setTab] = useState<StepTab>("basics");
  const [focusRoute, setFocusRoute] = useState<string | undefined>();
  const [freshStep, setFreshStep] = useState<string | null>(null);
  const [wide, setWide] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [call, setCall] = useState<CallInfo>(NO_CALL);
  const opener = useRef<HTMLElement | null>(null);

  const version = agent?.current_version?.number ?? 0;
  const renamed = !!agent && name.trim() !== agent.name;
  const redescribed = !!agent && description.trim() !== agent.description;
  const unpublished = !published || renamed || redescribed || canonical(toPayload(published)) !== canonical(toPayload(graph));
  const needSetup = graph.nodes.filter((n) => stepIssues(graph, n.id).length > 0).length;
  const flowProblems = flowIssues(graph);
  const problems = [...(name.trim() ? [] : ["Give the flow a name in Flow settings."]), ...validate(graph)];
  const callable = canCall(agent);
  // The call runs the published version, so the live view follows that graph.
  const live = useCallView(published ?? graph, call, client);
  const activeStep = graph.nodes.find((n) => live.view.nodes[n.id]?.state === "active")?.name;

  const state: { tone: "neutral" | "good" | "bad"; text: string } =
    graph.nodes.length === 0
      ? { tone: "neutral", text: "No steps yet" }
      : needSetup > 0
        ? { tone: "bad", text: `${needSetup} ${needSetup === 1 ? "step needs" : "steps need"} setup` }
        : flowProblems.length > 0
          ? { tone: "bad", text: flowProblems[0] }
          : { tone: "good", text: unpublished ? "Ready to publish" : "Published" };

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
  const openStep = (g: GraphDraft, id: string, forced?: StepTab) => {
    const issues = stepIssues(g, id);
    const sameStep = panel?.kind === "step" && panel.id === id;
    setTab(forced ?? issues[0]?.tab ?? (sameStep ? tab : "basics"));
    if (!forced) setFocusRoute(undefined);
    setFreshStep(null);
    open({ kind: "step", id });
  };
  const addStep = () => {
    const next = addNode(graph, uniqueName(graph, "New step"));
    if (next === graph) return;
    const id = next.nodes[next.nodes.length - 1].id;
    setGraph(next);
    openStep(next, id, "basics");
    setFreshStep(id);
  };
  const drawRoute = (from: string, to: string) => {
    if (call.live) return;
    const next = addDraftRoute(graph, from, to);
    if (next === graph) return;
    setGraph(next);
    setFocusRoute(next.edges[next.edges.length - 1].id);
    openStep(next, from, "routes");
  };
  const select = (s: Selection) => {
    if (call.live) return;
    if (s?.kind === "node") openStep(graph, s.id);
    else if (s?.kind === "edge") {
      const e = graph.edges.find((x) => x.id === s.id);
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
      enabled: agent?.enabled ?? true,
      config: toPayload(graph) as CreateAgentInput["config"],
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
        <span className="chip">{agent ? `v${version}${unpublished ? " · unpublished changes" : ""}` : "Never published"}</span>
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
          title={!agent ? "Publish once to test" : !callable ? "This flow is not ready to take calls" : undefined}
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
        {graph.nodes.length > 0 && (
          <button
            type="button"
            className="action-button-small flow-editor__add"
            disabled={graph.nodes.length >= MAX_NODES || call.live}
            title={graph.nodes.length >= MAX_NODES ? `A flow has at most ${MAX_NODES} steps` : undefined}
            onClick={addStep}
          >
            Add step
          </button>
        )}
        <span className="subtext">
          {graph.nodes.length} of {MAX_NODES} steps
        </span>
        <span className="grow" />
        {graph.nodes.length > 0 && !call.live && <span className="subtext">Drag from the dot on a step's edge to add a route</span>}
      </div>

      <div className={stageClass}>
        {graph.nodes.length === 0 ? (
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
            selected={panel?.kind === "step" ? { kind: "node", id: panel.id } : null}
            onSelect={select}
            onMove={(id, position) => setGraph((g) => updateNode(g, id, { position }))}
            onConnect={drawRoute}
            visual={call.live ? live.view.nodes : undefined}
            traversedEdges={call.live ? live.traversedEdges : undefined}
            justHandedEdge={live.justHanded}
          />
        )}

        {panel?.kind === "step" && (
          <StepPanel
            key={panel.id}
            graph={graph}
            stepId={panel.id}
            providers={providers}
            flowTools={flowTools}
            tab={tab}
            onTab={setTab}
            focusRoute={focusRoute}
            onFocusRoute={setFocusRoute}
            focusName={freshStep === panel.id}
            wide={wide}
            onToggleWide={() => setWide((w) => !w)}
            onChange={setGraph}
            onClose={close}
            returnFocusTo={opener.current}
            fallbackFocusId="flow-editor-title"
          />
        )}
        {panel?.kind === "settings" && (
          <FlowSettingsPanel
            published={
              agent?.current_version
                ? { number: agent.current_version.number, createdAt: agent.current_version.created_at, hash: agent.current_version.config_hash }
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
        {panel?.kind === "call" && agent && (
          <SidePanel title="Test call" tone="dark" onClose={close} returnFocusTo={opener.current} fallbackFocusId="flow-editor-title">
            {unpublished && (
              <p role="note" className="note">
                This call runs v{version}, the published version. Publish to hear your changes.
              </p>
            )}
            <AgentVoiceTest agent={agent} client={client} keyValue={keyValue} onCall={setCall} activeStep={activeStep} />
          </SidePanel>
        )}
      </div>

      {confirming && (
        <PublishDialog
          flowName={name}
          version={version + 1}
          firstVersion={!published}
          changes={[
            ...(renamed ? [`Renamed the flow to ${name.trim()}`] : []),
            ...(redescribed ? ["Description changed"] : []),
            ...describeChanges(published, graph),
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
