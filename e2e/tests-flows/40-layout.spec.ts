import { test, expect } from "../lib/flows-stack";
import { FLOW, api, connectAdmin, openFlow } from "../lib/flows-helpers";

// Demo-path screens must not scroll sideways at phone width. Uses the flow and the call the
// lifecycle spec left behind.
test.describe.configure({ mode: "serial" });

test("layout: demo-path screens do not scroll sideways at 390px", async ({ page, stack }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await connectAdmin(page, stack);
  const noSideways = async (where: string) => {
    const { sw, iw } = await page.evaluate(() => ({ sw: document.documentElement.scrollWidth, iw: window.innerWidth }));
    expect(sw, `${where} scrolls sideways (${sw} > ${iw})`).toBeLessThanOrEqual(iw);
  };
  // Client-side navigation keeps the management key (a reload would clear it).
  const go = async (path: string) => {
    await page.evaluate((p) => {
      history.pushState({}, "", p);
      dispatchEvent(new PopStateEvent("popstate"));
    }, path);
  };
  await noSideways("dashboard");
  await go("/providers");
  await expect(page.getByRole("heading", { name: "Providers", level: 1 })).toBeVisible();
  await noSideways("providers");
  await go("/mcp-servers");
  await expect(page.getByRole("heading", { name: "MCP tools", level: 1 })).toBeVisible();
  await noSideways("mcp tools");
  await go("/sessions");
  await expect(page.getByRole("heading", { name: "Calls", level: 1 })).toBeVisible();
  await noSideways("calls");
  await go("/flows");
  await expect(page.getByRole("heading", { name: "Flows", level: 1 })).toBeVisible();
  await noSideways("flows");
  await openFlow(page, FLOW);
  await noSideways("flow editor");
  const sessions = await api<{ data: { id: string }[] }>(stack, "/sessions?limit=1");
  if (sessions.data.length > 0) {
    await go(`/sessions/${sessions.data[0].id}`);
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
    await noSideways("call detail");
  }
});
