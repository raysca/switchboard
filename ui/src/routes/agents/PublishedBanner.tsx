import { Link } from "react-router";

export type PublishedBannerProps = { agentId: string; agentName: string; version: number; olderCalls: number; onDismiss: () => void };

/** Said once after a publish: what is live now, and what is still finishing on the old version. */
export function PublishedBanner({ agentId, agentName, version, olderCalls, onDismiss }: PublishedBannerProps) {
  return (
    <div role="status" aria-label="Publish confirmation" className="published-banner">
      <span className="lamp" data-tone="ok" aria-hidden="true" />
      <p>
        <b>Published v{version}.</b> {agentName.trim() || "This agent"} now answers new calls with it.
        {olderCalls > 0 && ` ${olderCalls} ${olderCalls === 1 ? "call is" : "calls are"} finishing on an earlier version and will end on their own.`}
      </p>
      <Link to={`/sessions?agent=${encodeURIComponent(agentId)}&outcome=live`} className="action-button-small">
        Watch calls
      </Link>
      <button type="button" className="icon-button" aria-label="Dismiss" onClick={onDismiss}>
        ×
      </button>
    </div>
  );
}
