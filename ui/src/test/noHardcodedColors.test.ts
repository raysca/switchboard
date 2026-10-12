import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

// Files already moved to tokens. Each task appends the files it converted.
export const CONVERTED: string[] = ["app/App.tsx", "routes/sections.tsx", "components/primitives.tsx", "routes/dashboard/DashboardView.tsx", "routes/providers/ProvidersView.tsx", "routes/agents/AgentVoiceTest.tsx", "routes/agents/graph/LiveGraph.tsx", "routes/agents/graph/Canvas.tsx", "routes/agents/AgentsView.tsx", "routes/agents/StageFields.tsx", "components/debug/PipecatDebugPanel.tsx", "routes/sessions/SessionsView.tsx", "routes/sessions/SessionDetailView.tsx", "components/timeline/TurnTimeline.tsx", "routes/mcp/MCPView.tsx", "routes/server/ServerView.tsx", "routes/onboarding/OnboardingView.tsx", "components/SidePanel.tsx", "routes/agents/graph/StepPanel.tsx", "routes/agents/graph/StepAdvanced.tsx", "routes/agents/graph/FlowSettingsPanel.tsx", "routes/agents/graph/PublishDialog.tsx", "routes/agents/graph/FlowEditor.tsx", "components/Modal.tsx", "routes/agents/NewFlowDialog.tsx", "routes/agents/ArchiveFlowDialog.tsx"];

const colour = /#[0-9a-fA-F]{3,8}\b|rgba?\(/;

describe("no hard-coded colours outside the token block", () => {
  for (const f of CONVERTED) {
    it(f, () => {
      const lines = readFileSync(resolve(__dirname, "..", f), "utf8").split("\n");
      const bad = lines.map((l, i) => [i + 1, l] as const).filter(([, l]) => colour.test(l));
      expect(bad).toEqual([]);
    });
  }
  it("styles.css defines colours only in the :root token block", () => {
    const css = readFileSync(resolve(__dirname, "../styles.css"), "utf8");
    const after = css.split("/* end tokens */")[1] ?? css;
    const leaks = after.split("\n").filter((l) => colour.test(l) && !l.trim().startsWith("/*"));
    expect(leaks).toEqual([]);
  });
});
