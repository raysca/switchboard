import { useParams } from "react-router";

// Placeholder so the fleet row link resolves; the Test suite and History tabs replace it.
export function AgentEvaluation() {
  const { agentId } = useParams();
  return (
    <header className="page-head">
      <h1 tabIndex={-1}>Evaluation: {agentId}</h1>
    </header>
  );
}
