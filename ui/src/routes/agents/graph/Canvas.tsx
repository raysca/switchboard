import { useEffect, useMemo, useState } from "react";
import {
  Background,
  Controls,
  Handle,
  MarkerType,
  Position,
  ReactFlow,
  applyNodeChanges,
  type Edge,
  type Node,
  type NodeChange,
  type NodeProps,
  type NodeTypes,
} from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import { isUnreachable, stepIssues, type GraphDraft } from "./model";

export type NodeState = "idle" | "preparing" | "active" | "visited";
export type Activity = "listening" | "thinking" | "speaking" | "tool" | "interrupted" | "error" | null;
export type NodeVisual = { state: NodeState; activity: Activity };
export type Selection = { kind: "node" | "edge"; id: string } | null;

type AgentNodeData = {
  name: string;
  model: string;
  voice: string;
  entry: boolean;
  setup: "ready" | "incomplete" | "unreachable" | null;
  visual?: NodeVisual;
};

const ACTIVITY_LABEL: Record<Exclude<Activity, null>, string> = {
  listening: "Listening",
  thinking: "Thinking",
  speaking: "Speaking",
  tool: "Running a tool",
  interrupted: "Interrupted",
  error: "Error",
};
const SETUP_LABEL = { ready: "Ready", incomplete: "Needs setup", unreachable: "No route leads here" } as const;

function AgentNode({ data, selected }: NodeProps<Node<AgentNodeData>>) {
  const state = data.visual?.state ?? "idle";
  const activity = data.visual?.activity ?? null;
  const status =
    state === "preparing"
      ? "Preparing…"
      : state === "active"
        ? activity
          ? ACTIVITY_LABEL[activity]
          : "On the call"
        : state === "visited"
          ? "Visited"
          : data.setup
            ? SETUP_LABEL[data.setup]
            : null;
  const incomplete = state === "idle" && data.setup === "incomplete";
  return (
    <div
      className={`agent-node agent-node--${state}${incomplete ? " agent-node--incomplete" : ""}${selected ? " agent-node--selected" : ""}`}
      data-state={state}
    >
      <Handle type="target" position={Position.Left} />
      <div className="agent-node__title">
        {data.name || "Untitled step"}
        {data.entry && <span className="agent-node__badge">Start</span>}
      </div>
      <div className="agent-node__meta">{data.model && data.voice ? `${data.model} · ${data.voice}` : data.model || "No model or voice yet"}</div>
      {status && <div className="agent-node__status">{status}</div>}
      <Handle type="source" position={Position.Right} />
    </div>
  );
}

const nodeTypes: NodeTypes = { agent: AgentNode };

const str = (v: unknown): string => (typeof v === "string" ? v : "");
const stage = (c: Record<string, unknown>, k: string): Record<string, unknown> =>
  c[k] && typeof c[k] === "object" ? (c[k] as Record<string, unknown>) : {};

/** Routes as React Flow edges: a route with no condition yet draws as a labelled draft. */
export function flowEdges(graph: GraphDraft, selected?: Selection, traversedEdges?: string[], justHandedEdge?: string): Edge[] {
  return graph.edges.map((e) => ({
    id: e.id,
    source: e.from,
    target: e.to,
    label: !e.condition.trim() ? "Set a condition" : e.condition.length > 40 ? `${e.condition.slice(0, 37)}…` : e.condition,
    animated: traversedEdges?.includes(e.id) ?? false,
    selected: selected?.kind === "edge" && selected.id === e.id,
    markerEnd: { type: MarkerType.ArrowClosed },
    className:
      [
        !e.condition.trim() ? "agent-edge--draft" : "",
        traversedEdges?.includes(e.id) ? `agent-edge--traversed${e.id === justHandedEdge ? " agent-edge--just" : ""}` : "",
      ]
        .join(" ")
        .trim() || undefined,
  }));
}

export type CanvasProps = {
  graph: GraphDraft;
  selected?: Selection;
  onSelect?: (s: Selection) => void;
  onMove?: (id: string, pos: { x: number; y: number }) => void;
  onConnect?: (from: string, to: string) => void;
  /** Live view: per-node state and the handoffs the call has taken. */
  visual?: Record<string, NodeVisual>;
  traversedEdges?: string[];
  /** The handoff taken a moment ago: its edge draws in. */
  justHandedEdge?: string;
  readOnly?: boolean;
  height?: number;
};

export function Canvas({ graph, selected, onSelect, onMove, onConnect, visual, traversedEdges, justHandedEdge, readOnly, height = 420 }: CanvasProps) {
  const derived = useMemo<Node<AgentNodeData>[]>(
    () =>
      graph.nodes.map((n) => ({
        id: n.id,
        type: "agent",
        position: n.position,
        selected: selected?.kind === "node" && selected.id === n.id,
        data: {
          name: n.name,
          model: str(stage(n.config, "llm").model),
          voice: str(stage(n.config, "tts").voice),
          entry: n.id === graph.entry,
          setup: readOnly ? null : stepIssues(graph, n.id).length ? "incomplete" : isUnreachable(graph, n.id) ? "unreachable" : "ready",
          visual: visual?.[n.id],
        },
      })),
    [graph, selected, visual, readOnly],
  );
  // Dragging updates local node state; the graph (the source of truth) is told at drag stop.
  const [nodes, setNodes] = useState(derived);
  useEffect(() => setNodes(derived), [derived]);

  const edges = useMemo(() => flowEdges(graph, selected, traversedEdges, justHandedEdge), [graph.edges, selected, traversedEdges, justHandedEdge]);


  return (
    <div className="agent-canvas" style={{ height }} data-testid="agent-canvas">
      <ReactFlow
        // Re-fit when a step is added or removed, so a new step is never left off-screen.
        key={graph.nodes.length}
        nodes={nodes}
        edges={edges}
        nodeTypes={nodeTypes}
        onNodesChange={(changes: NodeChange<Node<AgentNodeData>>[]) => {
          setNodes((ns) => applyNodeChanges(changes, ns));
          // Enter or Space on a focused step selects it; React Flow reports that as a change, not a click.
          for (const c of changes) if (c.type === "select" && c.selected) onSelect?.({ kind: "node", id: c.id });
        }}
        selectNodesOnDrag={false}
        // Steps are deleted from their panel, which updates the flow; React Flow's own Backspace
        // would only drop the step from the canvas.
        deleteKeyCode={null}
        onNodeDragStop={(_, node) => onMove?.(node.id, node.position)}
        onConnect={(c) => c.source && c.target && onConnect?.(c.source, c.target)}
        onNodeClick={(_, node) => onSelect?.({ kind: "node", id: node.id })}
        onEdgeClick={(_, edge) => onSelect?.({ kind: "edge", id: edge.id })}
        onPaneClick={() => onSelect?.(null)}
        nodesDraggable={!readOnly}
        nodesConnectable={!readOnly}
        elementsSelectable
        fitView
        fitViewOptions={{ maxZoom: 1 }}
        proOptions={{ hideAttribution: true }}
      >
        <Background />
        <Controls showInteractive={false} />
      </ReactFlow>
    </div>
  );
}
