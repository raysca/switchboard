import { test, expect } from "../lib/agents-stack";
import { AGENT, api, connectAdmin, openAgent } from "../lib/agents-helpers";

// Demo-path screens must not scroll sideways at phone width. Uses the agent and the call the
// lifecycle spec left behind.
test.describe.configure({ mode: "serial" });

test("layout: demo-path screens do not scroll sideways at 390px", async ({ page, stack }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await connectAdmin(page, stack);
  const noSideways = async (where: string) => {
    const { sw, iw, wide } = await page.evaluate(() => {
      const iw = window.innerWidth;
      // What widens the page: out-of-flow elements past the viewport (an absolutely positioned .visually-hidden cell is not
      // clipped by a scroller that is not its containing block).
      const wide = [...document.querySelectorAll("body *")]
        .map((el) => ({ el, r: el.getBoundingClientRect(), pos: getComputedStyle(el).position }))
        .filter((x) => x.r.right > iw && x.pos !== "static" && x.pos !== "relative")
        .slice(0, 4)
        .map(({ el, r, pos }) => `${el.tagName.toLowerCase()}.${String(el.className).split(" ")[0]} ${pos} right=${Math.round(r.right)} "${(el.textContent ?? "").trim().slice(0, 30)}"`);
      return { sw: document.documentElement.scrollWidth, iw, wide };
    });
    expect(sw, `${where} scrolls sideways (${sw} > ${iw}); widest: ${wide.join(" | ")}`).toBeLessThanOrEqual(iw);
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
  await go("/agents");
  await expect(page.getByRole("heading", { name: "Agents", level: 1 })).toBeVisible();
  await noSideways("agents");
  await openAgent(page, AGENT);
  await noSideways("agent editor");
  const sessions = await api<{ data: { id: string }[] }>(stack, "/sessions?limit=1");
  if (sessions.data.length > 0) {
    await go(`/sessions/${sessions.data[0].id}`);
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
    await noSideways("call detail");
  }
});
