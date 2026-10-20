import { test, expect, LIVE, PROVIDER } from "../lib/flows-stack";
import { step } from "../lib/evidence";
import { BUY, FLOW, FRONT, FRONT_PROMPT, LLM_MODEL, SALES, SALES_PROMPT, api, botReplies, botReply, catalogDefault, connectAdmin, loadProviders, openDesigner, openFlow, openFlowSettings, openFlowTest, openFlowsList, pick, sendText, stage, startCall, stepCard, timelineEvents } from "../lib/flows-helpers";

// A flow's life in a real browser against a real server. Two modes:
//  - fake providers (default, free): the fake LLM echoes the step's system prompt as a label and
//    calls a step's transfer tool on "transfer to <step>", so every step is deterministic;
//  - live xAI (E2E_PROVIDER=xai, make e2e-xai): real STT/LLM/TTS calls, so the model itself must
//    call the transfer tool. Assertions then use the prompts' marker words, not exact text.
// Designer, stored flow, call, route, live flow view, republish, a failed step build, the flows list
// and the calls list are exercised together. The first test creates the flow the rest use.
test.describe.configure({ mode: "serial" });

test.beforeAll(loadProviders);

test("designer: draw a two-step flow with a route and publish it", async ({ page, stack }) => {
  await openDesigner(page, stack);

  // The start step: model, voice and instructions on its Basics tab.
  await page.getByLabel("LLM provider").selectOption(PROVIDER);
  await pick(page, "LLM model", LLM_MODEL());
  await page.getByLabel("Instructions").fill(FRONT_PROMPT);
  await page.getByLabel("TTS provider").selectOption(PROVIDER);
  await pick(page, "Voice", catalogDefault("tts"));

  // A second step: it starts from the start step's model and voice; give it its own instructions.
  await page.getByRole("button", { name: "Add step" }).click();
  await page.getByLabel("Step name").fill("Sales");
  await page.getByLabel("Instructions").fill(SALES_PROMPT);
  await page.getByRole("button", { name: "Done" }).click();
  // A step added later is brought into view, not left off the edge of the canvas.
  await expect(stepCard(page, "Sales")).toBeInViewport();
  await expect(stepCard(page, "Main agent")).toBeInViewport();

  // The route between them, from the keyboard path on the Routes tab.
  await stepCard(page, "Main agent").click();
  await page.getByRole("tab", { name: /Routes/ }).click();
  // By role: a label wrapping a select includes its option texts, so getByLabel cannot match exactly.
  await page.getByRole("combobox", { name: "Route to", exact: true }).selectOption({ label: "Sales" });
  await page.getByRole("button", { name: "Add route" }).click();
  await page.getByLabel("Condition for the route to Sales").fill("The caller wants to buy a car or asks about prices.");

  // Flow settings: the name, speech-to-text, and the sound that fills the gap between steps (the server has "chime").
  await openFlowSettings(page);
  await page.getByLabel("Flow name").fill(FLOW);
  await page.getByLabel("STT provider").selectOption(PROVIDER);
  await pick(page, "STT model", catalogDefault("stt"));
  await page.getByText("Advanced", { exact: true }).click();
  await page.getByLabel("Sound name").fill("chime");
  await page.getByLabel("Start after (ms)").fill("50");
  await step(page, "flow designed");

  await page.getByRole("button", { name: "Publish", exact: true }).click();
  await page.getByRole("button", { name: "Publish v1" }).click();
  await openFlowTest(page, FLOW);
  await step(page, "routed flow published and ready");

  // The stored version is the flow the designer drew.
  const flows = await api<{ data: { name: string; current_version: { config: any } }[] }>(stack, "/flows");
  const cfg = flows.data.find((a) => a.name === FLOW)!.current_version.config;
  expect(cfg.steps.map((n: any) => n.name)).toEqual(["Main agent", "Sales"]);
  expect(cfg.steps[0].config.stt).toBeTruthy();
  expect(cfg.steps[1].config.stt).toBeUndefined();
  expect(cfg.steps[1].config.llm.system_prompt).toBe(SALES_PROMPT);
  // The dropdowns stored the catalog default, or the "Other…" value typed for the fake.
  expect(cfg.steps[0].config.llm.model).toBe(LLM_MODEL());
  expect(cfg.steps.map((n: any) => n.config.tts.voice)).toEqual([catalogDefault("tts"), catalogDefault("tts")]);
  expect(cfg.routes).toHaveLength(1);
  expect(cfg.start).toBe(cfg.steps[0].id);
  expect(cfg.route_cue).toEqual({ name: "chime", after_ms: 50 });
});

test("call: the front step routes the call to sales and the live flow follows", async ({ page, stack }) => {
  await connectAdmin(page, stack);
  await openFlowTest(page, FLOW);
  await startCall(page);
  await expect(stepCard(page, "Main agent").locator(".step-node")).toHaveAttribute("data-state", "active");

  await sendText(page, "hello");
  await expect(botReplies(page).filter({ hasText: LIVE ? /./ : FRONT }).first()).toBeVisible();
  await expect(botReplies(page).filter({ hasText: SALES })).toHaveCount(0);

  await sendText(page, BUY);
  // The new step speaks (the transfer, the farewell and the greeting come from the models),
  // and the flow shows the call moved over.
  await expect(botReply(page, SALES)).toBeVisible({ timeout: LIVE ? 90_000 : 20_000 });
  await expect(stepCard(page, "Sales").locator(".step-node")).toHaveAttribute("data-state", "active", { timeout: LIVE ? 90_000 : 15_000 });
  await expect(stepCard(page, "Main agent").locator(".step-node")).toHaveAttribute("data-state", "visited");
  await step(page, "call moved to sales");

  const salesBefore = await botReplies(page).filter({ hasText: SALES }).count();
  await sendText(page, "what do you sell");
  await expect.poll(() => botReplies(page).filter({ hasText: SALES }).count(), { timeout: LIVE ? 90_000 : 15_000 }).toBeGreaterThan(salesBefore);
  if (!LIVE) await expect(botReplies(page).filter({ hasText: /front desk.*what do you sell/i })).toHaveCount(0);

  // The route is recorded on the stored session, for replay: step ids, not just names.
  const sessionId = (await page.getByTestId("session-summary").locator("code").innerText()).trim();
  await expect
    .poll(async () => {
      const t = await api<{ data: { spans: { name: string; attributes?: { from_step?: string; to_step?: string } }[] } }>(stack, `/sessions/${sessionId}/traces`);
      const active = t.data.spans.find((s) => s.name === "route.active");
      return [t.data.spans.some((s) => s.name === "route.preparing"), !!active?.attributes?.from_step, !!active?.attributes?.to_step];
    })
    .toEqual([true, true, true]);

  // The cue played over the gap between the steps and stopped when sales spoke: the
  // server's session timeline has both events (the first call of this stack is session 1).
  await expect
    .poll(async () => {
      const events = await timelineEvents(stack);
      const stop = events.find((e) => e.type === "cue_stop");
      return [events.some((e) => e.type === "cue_start"), stop?.details];
    })
    .toEqual([true, "first_audio"]);

  await page.getByTestId("disconnect-btn").click();
  await expect(page.getByTestId("connection-state")).toHaveText(/disconnected/i);
});

test("designer: reopen the graph, edit it, republish; a new call gets the new version", async ({ page, stack }) => {
  await connectAdmin(page, stack);
  await openFlow(page, FLOW);
  await expect(page.getByRole("heading", { name: FLOW, level: 1 })).toBeVisible();
  await expect(stepCard(page, "Sales")).toBeVisible();
  // Backspace on a selected step must not drop it from the canvas while the flow still has it.
  // (The step is selected and its panel open, focus on the panel title.)
  await stepCard(page, "Sales").click();
  await page.keyboard.press("Backspace");
  await expect(stepCard(page, "Sales")).toBeVisible();
  await page.getByRole("button", { name: "Done" }).click();

  // A third step can be added and deleted again; then edit the route's condition.
  await page.getByRole("button", { name: "Add step" }).click();
  await page.getByLabel("Step name").fill("Support");
  await page.getByRole("button", { name: "Delete step" }).click();
  await stepCard(page, "Main agent").click();
  await page.getByRole("tab", { name: /Routes/ }).click();
  await page.getByLabel("Condition for the route to Sales").fill("The caller wants to buy a car, asks about prices, or wants a quote.");
  await step(page, "route condition edited");
  // The test call runs the canvas as a draft: the panel says the edit is not published.
  await page.getByRole("button", { name: "Test call" }).click();
  await expect(page.getByText(/This call runs your unpublished changes/)).toBeVisible();
  // The publish dialog is modal: Tab never reaches the page behind it (the browser may take focus
  // to its own toolbar, which leaves the page's activeElement as <body>), Escape closes it and
  // focus returns to Publish.
  await page.getByRole("button", { name: "Publish", exact: true }).click();
  const dialog = page.getByRole("dialog");
  for (let i = 0; i < 4; i++) {
    await page.keyboard.press("Tab");
    const outside = await dialog.evaluate((d) => {
      const a = document.activeElement;
      return !!a && a !== document.body && !d.contains(a);
    });
    expect(outside, "focus reached the page behind the dialog").toBe(false);
  }
  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Publish", exact: true })).toBeFocused();
  await page.getByRole("button", { name: "Publish", exact: true }).click();
  await page.getByRole("button", { name: "Publish v2" }).click();
  // Publishing leaves the editor open on the new version, so it can be tested at once.
  await expect(page.getByText(/Published v2/)).toBeVisible();
  await expect(page.getByRole("heading", { name: FLOW, level: 1 })).toBeVisible();
  await expect(page.getByText(/This call runs v/)).toHaveCount(0);

  const flows = await api<{ data: { name: string; current_version: { number: number; config: any } }[] }>(stack, "/flows");
  const v = flows.data.find((a) => a.name === FLOW)!.current_version;
  expect(v.number).toBe(2);
  expect(v.config.steps).toHaveLength(2);
  expect(v.config.routes[0].condition).toBe("The caller wants to buy a car, asks about prices, or wants a quote.");

  // Call from inside the editor: the editor's own flow shows the call moving to Sales.
  await startCall(page);
  await sendText(page, BUY);
  await expect(botReply(page, SALES)).toBeVisible({ timeout: LIVE ? 90_000 : 20_000 });
  await expect(stepCard(page, "Sales").locator(".step-node")).toHaveAttribute("data-state", "active", { timeout: LIVE ? 90_000 : 15_000 });
  await page.getByTestId("disconnect-btn").click();
});

// Fake only: a step that cannot be built needs a deliberately broken voice, which a
// real provider cannot be made to have on demand.
test("failure: a sales step that cannot be built leaves the caller with the front step", async ({ page, stack }) => {
  test.skip(LIVE, "needs the fake provider's brokenvoice");
  const body = {
    name: "E2E Broken Sales",
    config: {
      start: "front",
      steps: [
        { id: "front", name: "Front", config: { stt: stage({ model: "chirp" }), llm: stage({ model: "m", system_prompt: FRONT_PROMPT }), tts: stage({ voice: "frontvoice" }) } },
        { id: "sales", name: "Sales", config: { llm: stage({ model: "m", system_prompt: SALES_PROMPT }), tts: stage({ voice: "brokenvoice" }) } },
      ],
      routes: [{ id: "e1", from: "front", to: "sales", condition: "The caller wants to buy." }],
    },
  };
  await api(stack, "/flows", { method: "POST", body: JSON.stringify(body) });

  await connectAdmin(page, stack);
  await openFlowTest(page, "E2E Broken Sales");
  await startCall(page);

  await sendText(page, BUY);
  await expect(botReply(page, /Connecting you/)).toBeVisible();
  await sendText(page, "are you still there");
  await expect(botReply(page, /front desk.*are you still there/i)).toBeVisible();
  await expect(botReplies(page).filter({ hasText: SALES })).toHaveCount(0);
  await expect(stepCard(page, "Front").locator(".step-node")).toHaveAttribute("data-state", "active");
  await step(page, "route failed, caller kept by the front step");

  const sessionId = (await page.getByTestId("session-summary").locator("code").innerText()).trim();
  await expect
    .poll(async () => {
      const t = await api<{ data: { spans: { name: string }[] } }>(stack, `/sessions/${sessionId}/traces`);
      return t.data.spans.map((s) => s.name);
    })
    .toContain("route.failed");
  await page.getByTestId("disconnect-btn").click();
});

test("a plain flow still works unchanged", async ({ page, stack }) => {
  await api(stack, "/flows", {
    method: "POST",
    body: JSON.stringify({ name: "E2E Plain", config: { stt: stage(LIVE ? {} : { model: "chirp" }), llm: stage({ model: LLM_MODEL(), system_prompt: "You are PLAIN. Answer in one short sentence." }), tts: stage() } }),
  });
  await connectAdmin(page, stack);
  await openFlowTest(page, "E2E Plain");
  await startCall(page);
  await sendText(page, "hello");
  await expect(botReply(page, /./)).toBeVisible({ timeout: LIVE ? 60_000 : 15_000 });
  await expect(botReplies(page).filter({ hasText: /Connecting you/ })).toHaveCount(0);
  await page.getByTestId("disconnect-btn").click();
});

test("flows list: status, enable toggle without a new version, copy and archive", async ({ page, stack }) => {
  await connectAdmin(page, stack);
  await openFlowsList(page);
  const row = page.getByRole("link", { name: FLOW, exact: true }).locator("xpath=ancestor::li[1]");
  await expect(row).toContainText("Ready");
  await expect(row).toContainText("2 steps · 1 route");

  const current = async () => (await api<{ data: { name: string; current_version: { number: number } }[] }>(stack, "/flows")).data.find((a) => a.name === FLOW)!.current_version.number;
  const before = await current();
  // The box follows the server, not the click, so click and wait instead of uncheck()/check().
  await row.getByRole("checkbox", { name: `${FLOW} enabled` }).click();
  await expect(row).toContainText("Disabled");
  await row.getByRole("checkbox", { name: `${FLOW} enabled` }).click();
  await expect(row).toContainText("Ready");
  expect(await current(), "toggling Enabled must not publish a version").toBe(before);

  // Duplicate and Archive live in the row's "More actions" menu (a <details>), closed until opened.
  await row.locator("summary").click();
  await expect(row.getByRole("button", { name: `Duplicate ${FLOW}` })).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(row.getByRole("button", { name: `Duplicate ${FLOW}` })).toBeHidden();
  await expect(row.locator("summary")).toBeFocused();
  await row.locator("summary").click();
  await row.getByRole("button", { name: `Duplicate ${FLOW}` }).click();
  // The server names a copy "<name> copy" (server/flow/service.go, Duplicate).
  const copyName = `${FLOW} copy`;
  const copy = page.getByRole("link", { name: copyName, exact: true });
  await expect(copy).toBeVisible();
  const copyRow = copy.locator("xpath=ancestor::li[1]");
  await copyRow.locator("summary").click();
  await copyRow.getByRole("button", { name: `Archive ${copyName}` }).click();
  await page.getByRole("button", { name: "Archive flow" }).click();
  await expect(page.getByRole("link", { name: copyName, exact: true })).toHaveCount(0);
});

test("calls: the finished call is listed, with its path and a transcript labelled by step", async ({ page, stack }) => {
  await connectAdmin(page, stack);
  await page.getByRole("link", { name: "Calls", exact: true }).first().click();
  await expect(page.getByRole("heading", { name: "Calls", level: 1 })).toBeVisible();
  await page.getByRole("combobox", { name: "Flow", exact: true }).selectOption({ label: FLOW });
  // The call that just ended may still read Ending; the list refreshes every 10 seconds.
  const row = page.getByRole("row").filter({ hasText: FLOW }).first();
  await expect(row).toContainText("Ended", { timeout: 20_000 });
  await step(page, "calls list");
  await row.getByRole("link").click();
  await expect(page.getByRole("heading", { level: 1 })).toContainText("Call ");
  await expect(page.getByText("Main agent → Sales", { exact: true })).toBeVisible();
  const transcript = page.getByRole("list", { name: "Transcript" });
  await expect(transcript).toContainText("Routed Main agent → Sales");
  await expect(transcript.getByText(/^Sales · /).first()).toBeVisible();
  await step(page, "call detail, labelled by step");
});
