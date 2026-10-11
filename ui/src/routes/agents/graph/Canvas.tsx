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
import type { GraphDraft } from "./model";

export type NodeState = "idle" | "preparing" | "active" | "visited";
export type Activity = "listening" | "thinking" | "speaking" | "tool" | "interrupted" | "error" | null;
export type NodeVisual = { state: NodeState; activity: Activity };
export type Selection = { kind: "node" | "edge"; id: string } | null;

type AgentNodeData = { name: string; model: string; voice: string; entry: boolean; visual?: NodeVisual };

const ACTIVITY_LABEL: Record<Exclude<Activity, null>, string> = {
  listening: "listening",
  thinking: "thinking",
  speaking: "speaking",
  tool: "running a tool",
  interrupted: "interrupted",
  error: "error",
};

function AgentNode({ data, selected }: NodeProps<Node<AgentNodeData>>) {
  const state = data.visual?.state ?? "idle";
  const activity = data.visual?.activity ?? null;
  return (
    <div className={`agent-node agent-node--${state}${selected ? " agent-node--selected" : ""}`} data-state={state}>
      <Handle type="target" position={Position.Left} />
      <div className="agent-node__title">
        {data.name || "Untitled"}
        {data.entry && <span className="agent-node__badge">entry</span>}
      </div>
      <div className="agent-node__meta">{data.model || "no model"}</div>
      <div className="agent-node__meta">{data.voice || "no voice"}</div>
      {state === "preparing" && <div className="agent-node__status">preparing…</div>}
      {state === "active" && activity && <div className="agent-node__status">{ACTIVITY_LABEL[activity]}</div>}
      {state === "visited" && <div className="agent-node__status">left</div>}
      <Handle type="source" position={Position.Right} />
    </div>
  );
}

const nodeTypes: NodeTypes = { agent: AgentNode };

const str = (v: unknown): string => (typeof v === "string" ? v : "");
const stage = (c: Record<string, unknown>, k: string): Record<string, unknown> =>
  c[k] && typeof c[k] === "object" ? (c[k] as Record<string, unknown>) : {};

export type CanvasProps = {
  graph: GraphDraft;
  selected?: Selection;
  onSelect?: (s: Selection) => void;
  onMove?: (id: string, pos: { x: number; y: number }) => void;
  onConnect?: (from: string, to: string) => void;
  /** Live view: per-node state and the handoffs the call has taken. */
  visual?: Record<string, NodeVisual>;
  traversedEdges?: string[];
  readOnly?: boolean;
  height?: number;
};

export function Canvas({ graph, selected, onSelect, onMove, onConnect, visual, traversedEdges, readOnly, height = 420 }: CanvasProps) {
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
          visual: visual?.[n.id],
        },
      })),
    [graph.nodes, graph.entry, selected, visual],
  );
  // Dragging updates local node state; the graph (the source of truth) is told at drag stop.
  const [nodes, setNodes] = useState(derived);
  useEffect(() => setNodes(derived), [derived]);

  const edges = useMemo<Edge[]>(
    () =>
      graph.edges.map((e) => ({
        id: e.id,
        source: e.from,
        target: e.to,
        label: e.condition.length > 40 ? `${e.condition.slice(0, 37)}…` : e.condition,
        animated: traversedEdges?.includes(e.id) ?? false,
        selected: selected?.kind === "edge" && selected.id === e.id,
        markerEnd: { type: MarkerType.ArrowClosed },
        className: traversedEdges?.includes(e.id) ? "agent-edge--traversed" : undefined,
      })),
    [graph.edges, selected, traversedEdges],
  );

  return (
    <div className="agent-canvas" style={{ height }} data-testid="agent-canvas">
      <ReactFlow
        nodes={nodes}
        edges={edges}
        nodeTypes={nodeTypes}
        onNodesChange={(changes: NodeChange<Node<AgentNodeData>>[]) => setNodes((ns) => applyNodeChanges(changes, ns))}
        onNodeDragStop={(_, node) => onMove?.(node.id, node.position)}
        onConnect={(c) => c.source && c.target && onConnect?.(c.source, c.target)}
        onNodeClick={(_, node) => onSelect?.({ kind: "node", id: node.id })}
        onEdgeClick={(_, edge) => onSelect?.({ kind: "edge", id: edge.id })}
        onPaneClick={() => onSelect?.(null)}
        nodesDraggable={!readOnly}
        nodesConnectable={!readOnly}
        elementsSelectable
        fitView
        proOptions={{ hideAttribution: true }}
      >
        <Background />
        <Controls showInteractive={false} />
      </ReactFlow>
    </div>
  );
}
