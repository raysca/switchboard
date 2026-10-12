import { Link } from "react-router";

export type PublishedBannerProps = { flowId: string; flowName: string; version: number; olderCalls: number; onDismiss: () => void };

/** Said once after a publish: what is live now, and what is still finishing on the old version. */
export function PublishedBanner({ flowId, flowName, version, olderCalls, onDismiss }: PublishedBannerProps) {
  return (
    <div role="status" aria-label="Publish confirmation" className="published-banner">
      <span className="lamp" data-tone="ok" aria-hidden="true" />
      <p>
        <b>Published v{version}.</b> {flowName.trim() || "This flow"} now answers new calls with it.
        {olderCalls > 0 && ` ${olderCalls} ${olderCalls === 1 ? "call is" : "calls are"} finishing on an earlier version and will end on their own.`}
      </p>
      <Link to={`/sessions?flow=${encodeURIComponent(flowId)}&outcome=live`} className="action-button-small">
        Watch calls
      </Link>
      <button type="button" className="icon-button" aria-label="Dismiss" onClick={onDismiss}>
        ×
      </button>
    </div>
  );
}
