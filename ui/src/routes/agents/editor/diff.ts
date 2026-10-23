import type { ToolSelection } from "../../../api/client";
import type { AgentDraft, DraftHandoff } from "./model";

type Rec = Record<string, unknown>;
const rec = (v: unknown): Rec => (v && typeof v === "object" && !Array.isArray(v) ? (v as Rec) : {});
const s = (v: unknown): string => (typeof v === "string" ? v : "");
const same = (a: unknown, b: unknown) => JSON.stringify(a ?? null) === JSON.stringify(b ?? null);
const AGENT_SETTINGS = [
  ["stt", "listening"],
  ["vad_stop_ms", "turn-taking"],
  ["context", "memory"],
  ["recording", "recording"],
  ["stt_p99_ms", "late-transcript wait"],
  ["async_tools", "background tools"],
  ["idle", "silent-caller behavior"],
  ["audio", "audio"],
] as const;

/** Plain-language changes from the published version to the draft, for the publish dialog. */
export function describeChanges(before: AgentDraft | null, after: AgentDraft): string[] {
  if (!before) return [];
  const out: string[] = [];
  const nameIn = (g: AgentDraft, id: string) => g.steps.find((n) => n.id === id)?.name.trim() || "Untitled step";
  const old = new Map(before.steps.map((n) => [n.id, n]));
  const now = new Map(after.steps.map((n) => [n.id, n]));

  for (const n of after.steps) if (!old.has(n.id)) out.push(`Added step ${n.name}`);
  for (const n of before.steps) if (!now.has(n.id)) out.push(`Removed step ${n.name}`);
  for (const n of after.steps) {
    const o = old.get(n.id);
    if (!o) continue;
    const [ol, nl, ot, nt] = [rec(o.config.llm), rec(n.config.llm), rec(o.config.tts), rec(n.config.tts)];
    if (o.name !== n.name) out.push(`Renamed ${o.name} to ${n.name}`);
    if (s(ol.provider) !== s(nl.provider) || s(ol.model) !== s(nl.model)) out.push(`${n.name}: model ${s(ol.model) || "none"} → ${s(nl.model) || "none"}`);
    if (s(ot.provider) !== s(nt.provider) || s(ot.voice) !== s(nt.voice))
      out.push(`${n.name}: voice ${s(ot.voice) || s(ot.provider) || "none"} → ${s(nt.voice) || s(nt.provider) || "none"}`);
    if (s(ol.system_prompt) !== s(nl.system_prompt)) out.push(`${n.name}: instructions changed`);
    if (s(o.config.greeting).trim() !== s(n.config.greeting).trim()) out.push(`${n.name}: greeting changed`);
    if (!same(ol.fallbacks, nl.fallbacks) || !same(ot.fallbacks, nt.fallbacks) || !same(ol.max_parallel, nl.max_parallel) || !same(ol.temperature, nl.temperature) || !same(ol.top_p, nl.top_p) || !same(ol.max_output_tokens, nl.max_output_tokens) || !same(ot.clean_text, nt.clean_text) || !same(ot.voice_settings, nt.voice_settings))
      out.push(`${n.name}: advanced settings changed`);
    if (!same([...o.tools].sort(), [...n.tools].sort())) out.push(`${n.name}: tools changed`);
    if (!!o.end_call !== !!n.end_call) out.push(`${n.name}: end call ${n.end_call ? "enabled" : "disabled"}`);
  }

  const key = (e: DraftHandoff) => `${e.from}>${e.to}`;
  const oldHandoffs = new Map(before.handoffs.map((e) => [key(e), e]));
  const newHandoffs = new Map(after.handoffs.map((e) => [key(e), e]));
  for (const e of after.handoffs) {
    const o = oldHandoffs.get(key(e));
    if (!o) out.push(`New route ${nameIn(after, e.from)} → ${nameIn(after, e.to)}`);
    else if (o.condition !== e.condition) out.push(`Route ${nameIn(after, e.from)} → ${nameIn(after, e.to)}: condition changed`);
  }
  for (const e of before.handoffs) if (!newHandoffs.has(key(e))) out.push(`Removed route ${nameIn(before, e.from)} → ${nameIn(before, e.to)}`);

  if (before.start !== after.start) out.push(`Start step is now ${nameIn(after, after.start)}`);
  const oldStart = rec(old.get(before.start)?.config);
  const newStart = rec(now.get(after.start)?.config);
  for (const [k, label] of AGENT_SETTINGS) if (!same(oldStart[k], newStart[k])) out.push(`Flow settings: ${label} changed`);
  if (!same(before.handoff_cue, after.handoff_cue)) out.push("Flow settings: route sound changed");
  return out;
}

/** Tools added to or removed from the flow, and tools that became required or optional. */
export function describeToolChanges(before: ToolSelection[], after: ToolSelection[]): string[] {
  const name = (t: ToolSelection) => `${t.mcp_server_id}/${t.remote_name}`;
  const was = new Map(before.map((t) => [name(t), t]));
  const is = new Map(after.map((t) => [name(t), t]));
  const out: string[] = [];
  for (const [k, t] of is) {
    const o = was.get(k);
    if (!o) out.push(`Added tool ${k}`);
    else if (o.required !== t.required) out.push(`Tool ${k} is now ${t.required ? "required" : "optional"}`);
  }
  for (const k of was.keys()) if (!is.has(k)) out.push(`Removed tool ${k}`);
  return out;
}
