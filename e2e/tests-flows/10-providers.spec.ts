import { test, expect, LIVE, PROVIDER } from "../lib/flows-stack";
import { step } from "../lib/evidence";
import { api, connectAdmin, loadProviders, openDesigner, openFlowSettings, openFlowsList, providers, stage, type ProviderStatus } from "../lib/flows-helpers";

// Provider detection in the admin UI: the suite's provider is detected, only detected providers
// are offered, and a missing STT provider is explained. No flow is created here.
test.describe.configure({ mode: "serial" });

test.beforeAll(loadProviders);

test("providers: the suite's provider is detected and only detected providers are offered", async ({ page, stack }) => {
  // The server reads provider secrets from its environment (global setup); none goes through the API.
  expect(providers.find((p) => p.id === PROVIDER)?.detected).toBe(true);
  await openDesigner(page, stack);
  // Model and voice are on the step panel; speech-to-text is in Flow settings.
  for (const kind of ["llm", "tts", "stt"] as const) {
    if (kind === "stt") await openFlowSettings(page);
    const offered = await page.getByLabel(`${kind.toUpperCase()} provider`).locator("option").evaluateAll((os) => os.map((o) => (o as HTMLOptionElement).value));
    const available = providers.filter((p) => p.services[kind]?.available).map((p) => p.id);
    expect(offered, `${kind} provider options`).toEqual(["", ...available]);
    // The echo stack also gets placeholder google, xai and elevenlabs keys (global setup) so the dentistry demo's flow can be created (60-dental); only those three are tolerated.
    const demoOnly = LIVE ? [] : ["google", "xai", "elevenlabs"];
    expect(available.filter((id) => !demoOnly.includes(id)), "the child gets only the suite's provider variables").toEqual([PROVIDER]);
  }
});

test("providers: with no STT provider available the designer names the variables to set", async ({ page, stack }) => {
  // Same server, one service taken away in the response: what an operator without speech keys sees.
  await page.route("**/api/v1/providers", async (route) => {
    const res = await route.fetch();
    const body = (await res.json()) as { data: ProviderStatus[] };
    for (const p of body.data) {
      const stt = p.services.stt;
      if (stt?.available) Object.assign(stt, { available: false, reason: "missing_env", missing_env: ["E2E_MISSING_STT_KEY"] });
    }
    await route.fulfill({ response: res, json: body });
  });
  await openDesigner(page, stack);
  await openFlowSettings(page);
  const note = page.getByRole("note").filter({ hasText: "No STT provider is available" });
  await expect(note).toContainText("E2E_MISSING_STT_KEY");
  await expect(page.getByLabel("STT provider").locator("option")).toHaveCount(1);
  await step(page, "no STT provider: empty-state note");
});

test("providers page: the detected provider is listed with where its credential comes from, and no field for a key", async ({ page, stack }) => {
  await connectAdmin(page, stack);
  await page.getByRole("link", { name: "Providers", exact: true }).first().click();
  const name = providers.find((p) => p.id === PROVIDER)!.display_name;
  const row = page.getByRole("row").filter({ hasText: name });
  await expect(row).toContainText("Environment variable");
  await row.getByRole("button", { name }).click();
  await expect(page.getByRole("heading", { name, level: 2 })).toBeVisible();
  const panel = page.getByRole("complementary", { name });
  await expect(panel).toContainText("Credential found in the server environment");
  await expect(panel).toContainText("never stored or shown here");
  await expect(page.locator("main input, main textarea, main form")).toHaveCount(0);
  await step(page, "providers page");
});

test("flows list names the variables a not-ready flow is missing", async ({ page, stack }) => {
  // The suite's flow does not exist yet (spec 20 creates it), so this test makes its own; the echo stack
  // is always ready, so only this row's readiness is altered in the response (route-mocked, R7).
  const NAME = "E2E Not Ready";
  const created = await api<{ data: { id: string } }>(stack, "/flows", {
    method: "POST",
    body: JSON.stringify({ name: NAME, config: { stt: stage({ model: "chirp" }), llm: stage({ model: "m", system_prompt: "You help." }), tts: stage({ voice: "v" }) } }),
  });
  await page.route("**/api/v1/flows?*", async (route) => {
    const res = await route.fetch();
    const body = (await res.json()) as { data: { name: string; readiness: object }[] };
    for (const f of body.data) {
      if (f.name === NAME) f.readiness = { state: "not_ready", reasons: ["missing_credential_llm"], missing_env: ["E2E_MISSING_LLM_KEY", "E2E_MISSING_LLM_KEY"] };
    }
    await route.fulfill({ response: res, json: body });
  });
  await connectAdmin(page, stack);
  await openFlowsList(page);
  const row = page.getByRole("listitem").filter({ has: page.getByRole("link", { name: NAME, exact: true }) });
  await expect(row).toContainText("missing_credential_llm");
  await expect(row.getByText("E2E_MISSING_LLM_KEY")).toHaveCount(1);
  await expect(row).toContainText("Set E2E_MISSING_LLM_KEY");
  await step(page, "flows list: missing variables");
  await api(stack, `/flows/${created.data.id}/archive`, { method: "POST" });
});
