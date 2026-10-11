import type { ProviderStatus } from "../../../api/client";
import { StageFields, type StageKind, type StageValue } from "../StageFields";
import type { GraphEdge, GraphNode, NodeConfig } from "./model";

type NodeInspectorProps = {
  node: GraphNode;
  isEntry: boolean;
  providers: ProviderStatus[];
  onChange: (patch: Partial<Omit<GraphNode, "id">>) => void;
  onMakeEntry: () => void;
  onRemove: () => void;
};

const rec = (v: unknown): Record<string, unknown> =>
  v && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : {};
const str = (v: unknown): string => (typeof v === "string" ? v : "");

/** Edits one node: its name and its own LLM and voice (STT only on the entry node). */
export function NodeInspector({ node, isEntry, providers, onChange, onMakeEntry, onRemove }: NodeInspectorProps) {
  const setStage = (cap: StageKind, patch: Record<string, unknown>) => {
    const config: NodeConfig = { ...node.config, [cap]: { ...rec(node.config[cap]), ...patch } };
    onChange({ config });
  };
  const id = (suffix: string) => `node-${node.id}-${suffix}`;
  const stageFields = (cap: StageKind) => {
    const stage = rec(node.config[cap]);
    return (
      <fieldset className="stage-box">
        <legend>{cap === "stt" ? "Speech-to-text" : cap === "llm" ? "Language model" : "Voice"}</legend>
        <StageFields
          kind={cap}
          providers={providers}
          idPrefix={id(cap)}
          value={{ provider: str(stage.provider), model: str(stage.model), voice: str(stage.voice), language: str(stage.language) }}
          onChange={(v: StageValue) =>
            setStage(cap, {
              provider: v.provider,
              ...(cap === "tts" ? { voice: v.voice } : { model: v.model }),
              ...(cap === "stt" ? { language: v.language } : {}),
            })
          }
        />
        {cap === "llm" && (
          <label className="form-field" htmlFor={id("llm-prompt")}>
            <span>System prompt</span>
            <textarea id={id("llm-prompt")} rows={6} value={str(stage.system_prompt)} onChange={(e) => setStage("llm", { system_prompt: e.target.value })} />
          </label>
        )}
      </fieldset>
    );
  };

  return (
    <div className="form-card" aria-label={`Agent node ${node.name}`}>
      <label className="form-field" htmlFor={id("name")}>
        <span>Node name</span>
        <input id={id("name")} type="text" value={node.name} maxLength={64} onChange={(e) => onChange({ name: e.target.value })} />
      </label>
      <p className="subtext">
        {isEntry ? "Entry node: answers first and owns the call-level settings (speech-to-text)." : "Reached by handoff from another node."}
      </p>
      {isEntry && stageFields("stt")}
      {stageFields("llm")}
      {stageFields("tts")}
      <div style={{ display: "flex", gap: "8px", marginTop: "12px" }}>
        {!isEntry && (
          <>
            <button type="button" className="action-button-small" onClick={onMakeEntry}>
              Make entry node
            </button>
            <button type="button" className="action-button-small action-button-danger" onClick={onRemove}>
              Remove node
            </button>
          </>
        )}
      </div>
    </div>
  );
}

type EdgeInspectorProps = {
  edge: GraphEdge;
  fromName: string;
  toName: string;
  onChange: (condition: string) => void;
  onRemove: () => void;
};

/** Edits a handoff: the condition becomes the description of the transfer tool the model sees. */
export function EdgeInspector({ edge, fromName, toName, onChange, onRemove }: EdgeInspectorProps) {
  return (
    <div className="form-card" aria-label={`Handoff from ${fromName} to ${toName}`}>
      <p>
        <strong>{fromName}</strong> hands off to <strong>{toName}</strong>
      </p>
      <label className="form-field" htmlFor={`edge-${edge.id}-condition`}>
        <span>When should it hand off?</span>
        <textarea
          id={`edge-${edge.id}-condition`}
          rows={4}
          value={edge.condition}
          maxLength={512}
          onChange={(e) => onChange(e.target.value)}
          placeholder="e.g. The caller wants pricing or to buy."
        />
      </label>
      <button type="button" className="action-button-small action-button-danger" onClick={onRemove}>
        Remove handoff
      </button>
    </div>
  );
}
