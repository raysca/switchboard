import { Link } from "react-router";
import type { CredentialDTO, ProviderDefinition } from "../../../api/client";
import type { GraphEdge, GraphNode, NodeConfig } from "./model";

type Capability = "stt" | "llm" | "tts";

type NodeInspectorProps = {
  node: GraphNode;
  isEntry: boolean;
  providers: ProviderDefinition[];
  credentials: CredentialDTO[];
  onChange: (patch: Partial<Omit<GraphNode, "id">>) => void;
  onMakeEntry: () => void;
  onRemove: () => void;
};

const rec = (v: unknown): Record<string, unknown> =>
  v && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : {};
const str = (v: unknown): string => (typeof v === "string" ? v : "");

/** Edits one node: its name and its own LLM and voice (STT only on the entry node). */
export function NodeInspector({ node, isEntry, providers, credentials, onChange, onMakeEntry, onRemove }: NodeInspectorProps) {
  const setStage = (cap: Capability, patch: Record<string, unknown>) => {
    const config: NodeConfig = { ...node.config, [cap]: { ...rec(node.config[cap]), ...patch } };
    onChange({ config });
  };
  const id = (suffix: string) => `node-${node.id}-${suffix}`;
  const providerOptions = (cap: Capability) =>
    providers.filter((p) => p.id !== "mcp" && p.capabilities.includes(cap));

  const stageFields = (cap: Capability) => {
    const stage = rec(node.config[cap]);
    const provider = str(stage.provider);
    const options = credentials.filter((c) => c.provider === provider);
    return (
      <fieldset className="stage-box">
        <legend>{cap === "stt" ? "Speech-to-text" : cap === "llm" ? "Language model" : "Voice"}</legend>
        <label className="form-field" htmlFor={id(`${cap}-provider`)}>
          <span>{cap.toUpperCase()} provider</span>
          <select
            id={id(`${cap}-provider`)}
            value={provider}
            onChange={(e) => setStage(cap, { provider: e.target.value, credential_id: "" })}
          >
            <option value="">Select a provider</option>
            {providerOptions(cap).map((p) => (
              <option key={p.id} value={p.id}>
                {p.display_name}
              </option>
            ))}
          </select>
        </label>
        {cap === "stt" && (
          <label className="form-field" htmlFor={id("stt-model")}>
            <span>STT model</span>
            <input id={id("stt-model")} type="text" value={str(stage.model)} onChange={(e) => setStage("stt", { model: e.target.value })} />
          </label>
        )}
        {cap === "llm" && (
          <>
            <label className="form-field" htmlFor={id("llm-model")}>
              <span>LLM model</span>
              <input id={id("llm-model")} type="text" value={str(stage.model)} onChange={(e) => setStage("llm", { model: e.target.value })} />
            </label>
            <label className="form-field" htmlFor={id("llm-prompt")}>
              <span>System prompt</span>
              <textarea id={id("llm-prompt")} rows={6} value={str(stage.system_prompt)} onChange={(e) => setStage("llm", { system_prompt: e.target.value })} />
            </label>
          </>
        )}
        {cap === "tts" && (
          <label className="form-field" htmlFor={id("tts-voice")}>
            <span>Voice</span>
            <input id={id("tts-voice")} type="text" value={str(stage.voice)} onChange={(e) => setStage("tts", { voice: e.target.value })} />
          </label>
        )}
        <label className="form-field" htmlFor={id(`${cap}-credential`)}>
          <span>{cap.toUpperCase()} credential</span>
          <select
            id={id(`${cap}-credential`)}
            value={str(stage.credential_id)}
            disabled={!provider}
            onChange={(e) => setStage(cap, { credential_id: e.target.value })}
          >
            <option value="">Select a credential</option>
            {options.map((c) => (
              <option key={c.id} value={c.id}>
                {c.masked_value} ({c.validation_state})
              </option>
            ))}
          </select>
        </label>
        {provider && options.length === 0 && (
          <p className="subtext" role="note">
            No {cap.toUpperCase()} credentials for this provider. <Link to="/providers">Add one on the Providers page</Link>.
          </p>
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
