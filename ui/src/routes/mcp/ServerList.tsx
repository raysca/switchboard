import { Link } from "react-router";
import type { MCPServerDTO } from "../../api/client";
import { ago, serverStatus } from "./mcpStats";

export type ServerListProps = {
  servers: MCPServerDTO[];
  toolCounts: Record<string, number | undefined>;
  /** Per server: how many tools have a changed schema in some agent. */
  changed: Record<string, number>;
  selectedId: string | undefined;
  now: number;
};

/** Servers, one per row: a lamp, the name, the tool count, and one line about its state. */
export function ServerList({ servers, toolCounts, changed, selectedId, now }: ServerListProps) {
  return (
    <ul className="mcp-list">
      {servers.map((s) => {
        const st = serverStatus(s);
        const n = changed[s.id] ?? 0;
        const count = toolCounts[s.id];
        let note: string;
        let tone: "ok" | "bad" | undefined = st.tone === "idle" ? undefined : st.tone;
        if (n > 0) {
          note = `${n} ${n === 1 ? "schema" : "schemas"} changed`;
          tone = "bad";
        } else if (st.tone === "ok") {
          const synced = ago(s.last_discovered_at, now);
          note = synced ? `Synced ${synced}` : st.label;
        } else if (st.tone === "bad") {
          const lastOk = ago(s.last_connected_at, now);
          note = lastOk ? `${st.label} · last ok ${lastOk}` : st.label;
        } else {
          note = st.label;
        }
        return (
          <li key={s.id}>
            <Link to={`/mcp-servers/${encodeURIComponent(s.id)}`} aria-current={selectedId === s.id ? "page" : undefined} className="mcp-item">
              <span className="lamp" data-tone={tone} aria-hidden="true" />
              <b>{s.name}</b>
              <span className="mcp-item__count">{count === undefined ? "" : `${count} ${count === 1 ? "tool" : "tools"}`}</span>
              <span className="mcp-item__note" data-tone={tone}>
                {note}
              </span>
            </Link>
          </li>
        );
      })}
    </ul>
  );
}
