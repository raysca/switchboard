import { useState, type FormEvent, type ReactNode } from "react";
import type { AgentDTO, CreateAgentInput, CredentialDTO, ProviderDefinition } from "../../../api/client";
import { Canvas, type Selection } from "./Canvas";
import { EdgeInspector, NodeInspector } from "./Inspector";
import {
  MAX_NODES,
  addNode,
  connect,
  connectError,
  fromAgentConfig,
  removeEdge,
  removeNode,
  setEntry,
  toPayload,
  updateEdge,
  updateNode,
  validate,
  type GraphDraft,
} from "./model";

type GraphDesignerProps = {
  agent: AgentDTO | null;
  providers: ProviderDefinition[];
  credentials: CredentialDTO[];
  pending: boolean;
  submitLabel: string;
  onSubmit: (input: CreateAgentInput) => void;
  onCancel: () => void;
  /** Rendered under the canvas: the voice test with the live graph view. */
  testPanel?: ReactNode;
};

/** Draws an agent as a graph: nodes are agents, edges are handoffs. */
export function GraphDesigner({ agent, providers, credentials, pending, submitLabel, onSubmit, onCancel, testPanel }: GraphDesignerProps) {
  const [name, setName] = useState(agent?.name ?? "");
  const [description, setDescription] = useState(agent?.description ?? "");
  const [graph, setGraph] = useState<GraphDraft>(() => {
    const tools = (agent?.current_version?.mcp_tools ?? []).map((t) => `${t.mcp_server_id}/${t.remote_name}`);
    const g = fromAgentConfig(agent?.current_version?.config ?? {}, tools);
    return agent ? g : { ...g, nodes: g.nodes.map((n) => ({ ...n, name: "Main agent" })) };
  });
  const [selected, setSelected] = useState<Selection>(() => ({ kind: "node", id: graph.entry }));
  const [newName, setNewName] = useState("");
  const [from, setFrom] = useState("");
  const [to, setTo] = useState("");
  const [condition, setCondition] = useState("");
  const [showErrors, setShowErrors] = useState(false);

  const nameOf = (id: string) => graph.nodes.find((n) => n.id === id)?.name ?? id;
  const problems = [...(name.trim() ? [] : ["Agent name is required."]), ...validate(graph)];
  const handoffError = from && to ? connectError(graph, from, to, condition) : null;

  const node = selected?.kind === "node" ? graph.nodes.find((n) => n.id === selected.id) : undefined;
  const edge = selected?.kind === "edge" ? graph.edges.find((e) => e.id === selected.id) : undefined;

  function submit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault();
    if (problems.length) {
      setShowErrors(true);
      return;
    }
    onSubmit({
      name: name.trim(),
      description: description.trim(),
      enabled: agent?.enabled ?? true,
      config: toPayload(graph) as CreateAgentInput["config"],
    });
  }

  function addAgent() {
    const label = newName.trim();
    if (!label) return;
    const next = addNode(graph, label);
    setGraph(next);
    setSelected({ kind: "node", id: next.nodes[next.nodes.length - 1].id });
    setNewName("");
  }

  function addHandoff() {
    const next = connect(graph, from, to, condition);
    if (next === graph) return;
    setGraph(next);
    setSelected({ kind: "edge", id: next.edges[next.edges.length - 1].id });
    setCondition("");
  }

  return (
    <form onSubmit={submit} className="designer" noValidate>
      {showErrors && problems.length > 0 && (
        <div role="alert" className="message-panel" style={{ borderColor: "#a43e30" }}>
          <ul style={{ margin: 0 }}>
            {problems.map((p) => (
              <li key={p}>{p}</li>
            ))}
          </ul>
        </div>
      )}

      <div className="form-card">
        <label className="form-field" htmlFor="designer-name">
          <span>Agent name</span>
          <input id="designer-name" type="text" value={name} maxLength={128} onChange={(e) => setName(e.target.value)} placeholder="e.g. Front Desk Assistant" />
        </label>
        <label className="form-field" htmlFor="designer-description">
          <span>Description</span>
          <textarea id="designer-description" rows={2} value={description} maxLength={1024} onChange={(e) => setDescription(e.target.value)} />
        </label>
      </div>

      <div style={{ display: "flex", gap: "8px", alignItems: "end", margin: "16px 0 8px" }}>
        <label className="form-field" htmlFor="designer-new-node" style={{ flex: 1 }}>
          <span>New agent node</span>
          <input id="designer-new-node" type="text" value={newName} maxLength={64} onChange={(e) => setNewName(e.target.value)} placeholder="e.g. Sales" />
        </label>
        <button type="button" className="action-button-small" onClick={addAgent} disabled={!newName.trim() || graph.nodes.length >= MAX_NODES}>
          Add agent
        </button>
      </div>

      <div className="designer-layout">
        <div>
          <Canvas
            graph={graph}
            selected={selected}
            onSelect={setSelected}
            onMove={(id, position) => setGraph((g) => updateNode(g, id, { position }))}
            onConnect={(f, t) => {
              const next = connect(graph, f, t, `The caller needs ${nameOf(t)}.`);
              if (next !== graph) {
                setGraph(next);
                setSelected({ kind: "edge", id: next.edges[next.edges.length - 1].id });
              }
            }}
          />
          {testPanel}
        </div>
        <div>
          {node && (
            <NodeInspector
              key={node.id}
              node={node}
              isEntry={node.id === graph.entry}
              providers={providers}
              credentials={credentials}
              onChange={(patch) => setGraph((g) => updateNode(g, node.id, patch))}
              onMakeEntry={() => setGraph((g) => setEntry(g, node.id))}
              onRemove={() => {
                setGraph((g) => removeNode(g, node.id));
                setSelected({ kind: "node", id: graph.entry });
              }}
            />
          )}
          {edge && (
            <EdgeInspector
              key={edge.id}
              edge={edge}
              fromName={nameOf(edge.from)}
              toName={nameOf(edge.to)}
              onChange={(c) => setGraph((g) => updateEdge(g, edge.id, c))}
              onRemove={() => {
                setGraph((g) => removeEdge(g, edge.id));
                setSelected(null);
              }}
            />
          )}
          {!node && !edge && <p className="subtext">Select an agent or a handoff to edit it.</p>}
        </div>
      </div>

      <fieldset className="stage-box">
        <legend>Handoffs</legend>
        {graph.edges.length === 0 && <p className="subtext">No handoffs yet. Drag from one agent to another, or add one here.</p>}
        <ul>
          {graph.edges.map((e) => (
            <li key={e.id}>
              <button type="button" className="action-button-small" onClick={() => setSelected({ kind: "edge", id: e.id })}>
                {nameOf(e.from)} → {nameOf(e.to)}
              </button>{" "}
              {e.condition}
            </li>
          ))}
        </ul>
        <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: "8px" }}>
          <label className="form-field" htmlFor="handoff-from">
            <span>From</span>
            <select id="handoff-from" value={from} onChange={(e) => setFrom(e.target.value)}>
              <option value="">Select an agent</option>
              {graph.nodes.map((n) => (
                <option key={n.id} value={n.id}>
                  {n.name}
                </option>
              ))}
            </select>
          </label>
          <label className="form-field" htmlFor="handoff-to">
            <span>To</span>
            <select id="handoff-to" value={to} onChange={(e) => setTo(e.target.value)}>
              <option value="">Select an agent</option>
              {graph.nodes.map((n) => (
                <option key={n.id} value={n.id}>
                  {n.name}
                </option>
              ))}
            </select>
          </label>
        </div>
        <label className="form-field" htmlFor="handoff-condition">
          <span>Hand off when</span>
          <textarea id="handoff-condition" rows={2} value={condition} onChange={(e) => setCondition(e.target.value)} placeholder="e.g. The caller wants pricing or to buy." />
        </label>
        {handoffError && condition && <p role="note" className="subtext">{handoffError}</p>}
        <button type="button" className="action-button-small" onClick={addHandoff} disabled={!from || !to || !!handoffError}>
          Add handoff
        </button>
      </fieldset>

      {graph.edges.length > 0 && (
        <fieldset className="stage-box">
          <legend>Handoff sound</legend>
          <p className="subtext">Plays while the call is between agents. Sounds are set up by the server operator (audio.cues).</p>
          <div style={{ display: "grid", gridTemplateColumns: "2fr 1fr", gap: "8px" }}>
            <label className="form-field" htmlFor="cue-name">
              <span>Sound name</span>
              <input
                id="cue-name"
                type="text"
                value={graph.handoff_cue?.name ?? ""}
                placeholder="none"
                onChange={(e) =>
                  setGraph((g) => {
                    const v = e.target.value.trim();
                    const { handoff_cue: _drop, ...rest } = g;
                    return v ? { ...rest, handoff_cue: { ...(g.handoff_cue ?? {}), name: v } } : rest;
                  })
                }
              />
            </label>
            <label className="form-field" htmlFor="cue-after">
              <span>Start after (ms)</span>
              <input
                id="cue-after"
                type="number"
                min={0}
                value={graph.handoff_cue?.after_ms ?? ""}
                disabled={!graph.handoff_cue}
                onChange={(e) =>
                  setGraph((g) =>
                    g.handoff_cue
                      ? { ...g, handoff_cue: { name: g.handoff_cue.name, ...(Number(e.target.value) > 0 ? { after_ms: Number(e.target.value) } : {}) } }
                      : g,
                  )
                }
              />
            </label>
          </div>
        </fieldset>
      )}

      <div style={{ display: "flex", gap: "8px", marginTop: "16px" }}>
        <button type="submit" className="action-button" disabled={pending}>
          {pending ? "Saving…" : submitLabel}
        </button>
        <button type="button" className="action-button-small" onClick={onCancel}>
          Cancel
        </button>
      </div>
    </form>
  );
}
