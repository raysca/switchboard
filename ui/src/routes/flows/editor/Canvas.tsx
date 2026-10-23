import { useEffect, useMemo, useRef, useState } from "react";
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
  type ReactFlowInstance,
} from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import { isUnreachable, stepIssues, type FlowDraft } from "./model";

export type StepState = "idle" | "preparing" | "active" | "visited";
export type Activity = "listening" | "thinking" | "speaking" | "tool" | "interrupted" | "error" | null;
export type StepVisual = { state: StepState; activity: Activity };
export type Selection = { kind: "step" | "route"; id: string } | null;

type StepNodeData = {
  name: string;
  model: string;
  voice: string;
  start: boolean;
  setup: "ready" | "incomplete" | "unreachable" | null;
  visual?: StepVisual;
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

function StepNode({ data, selected }: NodeProps<Node<StepNodeData>>) {
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
      className={`step-node step-node--${state}${incomplete ? " step-node--incomplete" : ""}${selected ? " step-node--selected" : ""}`}
      data-state={state}
    >
      <Handle type="target" position={Position.Left} />
      <div className="step-node__title">
        <b>{data.name || "Untitled step"}</b>
        {data.start && <span className="step-node__tag">Start</span>}
      </div>
      <div className="step-node__meta">{data.model && data.voice ? `${data.model} · ${data.voice}` : data.model || "No model or voice yet"}</div>
      {status && (
        <div className="step-node__status">
          <span className="step-node__dot" data-shape={state === "idle" && (data.setup === "incomplete" || data.setup === "unreachable") ? "square" : undefined} aria-hidden="true" />
          {status}
        </div>
      )}
      <Handle type="source" position={Position.Right} />
    </div>
  );
}

const nodeTypes: NodeTypes = { step: StepNode };

const str = (v: unknown): string => (typeof v === "string" ? v : "");
const stage = (c: Record<string, unknown>, k: string): Record<string, unknown> =>
  c[k] && typeof c[k] === "object" ? (c[k] as Record<string, unknown>) : {};

/** Routes as React Flow routes: a route with no condition yet draws as a labelled draft. */
export function flowRoutes(graph: FlowDraft, selected?: Selection, traversedRoutes?: string[], justHandedRoute?: string): Edge[] {
  return graph.handoffs.map((e) => ({
    id: e.id,
    source: e.from,
    target: e.to,
    label: !e.condition.trim() ? "Set a condition" : e.condition.length > 40 ? `${e.condition.slice(0, 37)}…` : e.condition,
    animated: traversedRoutes?.includes(e.id) ?? false,
    selected: selected?.kind === "route" && selected.id === e.id,
    markerEnd: { type: MarkerType.ArrowClosed },
    className:
      [
        !e.condition.trim() ? "route-edge--draft" : "",
        traversedRoutes?.includes(e.id) ? `route-edge--traversed${e.id === justHandedRoute ? " route-edge--just" : ""}` : "",
      ]
        .join(" ")
        .trim() || undefined,
  }));
}

export type CanvasProps = {
  graph: FlowDraft;
  selected?: Selection;
  onSelect?: (s: Selection) => void;
  onMove?: (id: string, pos: { x: number; y: number }) => void;
  onConnect?: (from: string, to: string) => void;
  /** Live view: per-step state and the routes the call has taken. */
  visual?: Record<string, StepVisual>;
  traversedRoutes?: string[];
  /** The route taken a moment ago: its route draws in. */
  justHandedRoute?: string;
  readOnly?: boolean;
  height?: number | string;
  /** The flow refits when this changes (a panel opened, closed or widened) as well as when a step is added or removed. */
  fitKey?: string;
};

export function Canvas({ graph, selected, onSelect, onMove, onConnect, visual, traversedRoutes, justHandedRoute, readOnly, height = 420, fitKey = "" }: CanvasProps) {
  const derived = useMemo<Node<StepNodeData>[]>(
    () =>
      graph.steps.map((n) => ({
        id: n.id,
        type: "step",
        position: n.position,
        selected: selected?.kind === "step" && selected.id === n.id,
        data: {
          name: n.name,
          model: str(stage(n.config, "llm").model),
          voice: str(stage(n.config, "tts").voice),
          start: n.id === graph.start,
          setup: readOnly ? null : stepIssues(graph, n.id).length ? "incomplete" : isUnreachable(graph, n.id) ? "unreachable" : "ready",
          visual: visual?.[n.id],
        },
      })),
    [graph, selected, visual, readOnly],
  );
  // Dragging updates local step state; the flow (the source of truth) is told at drag stop.
  // Adopted during render, not in an effect: a deleted step leaves the DOM in the same commit that closes its panel,
  // so the panel's focus return can tell that the step it came from is gone.
  const [steps, setNodes] = useState(derived);
  const [prevDerived, setPrevDerived] = useState(derived);
  if (prevDerived !== derived) {
    setPrevDerived(derived);
    setNodes(derived);
  }

  const routes = useMemo(() => flowRoutes(graph, selected, traversedRoutes, justHandedRoute), [graph.handoffs, selected, traversedRoutes, justHandedRoute]);

  // Re-fit in place (not by remounting, which would drop focus on a step and the viewport) when a step is added or
  // removed, or the room for the flow changes, so no step is left off-screen or tiny.
  const flow = useRef<ReactFlowInstance | null>(null);
  const fitted = useRef(`${graph.steps.length}:${fitKey}`);
  useEffect(() => {
    const key = `${graph.steps.length}:${fitKey}`;
    if (fitted.current === key) return;
    fitted.current = key;
    // A beat after the layout change: React Flow measures its container through a ResizeObserver.
    const id = setTimeout(() => void flow.current?.fitView({ maxZoom: 1, duration: 0 }), 80);
    return () => clearTimeout(id);
  }, [graph.steps.length, fitKey]);


  return (
    <div className="flow-canvas" style={{ height }} data-testid="flow-canvas" data-fit={`${graph.steps.length}:${fitKey}`}>
      <ReactFlow
        onInit={(instance) => {
          flow.current = instance as unknown as ReactFlowInstance;
        }}
        nodes={steps}
        edges={routes}
        nodeTypes={nodeTypes}
        onNodesChange={(changes: NodeChange<Node<StepNodeData>>[]) => {
          setNodes((ns) => applyNodeChanges(changes, ns));
          // Enter or Space on a focused step selects it; React Flow reports that as a change, not a click.
          for (const c of changes) if (c.type === "select" && c.selected) onSelect?.({ kind: "step", id: c.id });
        }}
        selectNodesOnDrag={false}
        // Steps are deleted from their panel, which updates the flow; React Flow's own Backspace
        // would only drop the step from the canvas.
        deleteKeyCode={null}
        onNodeDragStop={(_, step) => onMove?.(step.id, step.position)}
        onConnect={(c) => c.source && c.target && onConnect?.(c.source, c.target)}
        onNodeClick={(_, step) => onSelect?.({ kind: "step", id: step.id })}
        onEdgeClick={(_, route) => onSelect?.({ kind: "route", id: route.id })}
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
