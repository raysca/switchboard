import { test, expect, LIVE } from "../lib/agents-stack";
import { api, connectAdmin, openAgent, openAgentSettings, stage, stepCard } from "../lib/agents-helpers";

// Per-agent conversation settings (Phase 7b). Each test creates its own agent, so it runs alone with --grep.
test.skip(LIVE, "echo-provider spec");

type Step = { id: string; config: { tts?: { clean_text?: boolean }; failover?: { on?: string[] } } };
type Agent = { id: string; name: string; current_version: { config: { steps: Step[] } } };
const find = async (stack: Parameters<typeof api>[0], name: string) => (await api<{ data: Agent[] }>(stack, "/agents")).data.find((f) => f.name === name)!;

test("text cleaning and fallback rules are agent settings that survive a reload", async ({ page, stack }) => {
  const name = "E2E Call-wide Settings";
  const cfg = (extra: object = {}) => ({ llm: stage({ model: "m", system_prompt: "You help." }), tts: stage({ voice: "v" }), ...extra });
  await api(stack, "/agents", {
    method: "POST",
    body: JSON.stringify({
      name,
      config: {
        start: "front",
        steps: [
          { id: "front", name: "Front", config: { stt: stage({ model: "chirp" }), ...cfg() } },
          { id: "sales", name: "Sales", config: cfg() },
        ],
        handoffs: [{ id: "e1", from: "front", to: "sales", condition: "wants to buy" }],
      },
    }),
  });
  await connectAdmin(page, stack);
  await openAgent(page, name);
  await openAgentSettings(page);
  await page.locator("details.advanced").getByText("Advanced", { exact: true }).click();
  await page.getByLabel(/Clean text before speaking/).uncheck();
  await page.getByLabel("Rejected requests (bad request, bad key)", { exact: true }).check();
  await page.getByRole("button", { name: "Done" }).click();
  await page.getByRole("button", { name: "Publish", exact: true }).click();
  await page.getByRole("button", { name: "Publish v2" }).click();

  await expect
    .poll(async () => (await find(stack, name)).current_version.config.steps[0].config.tts?.clean_text)
    .toBe(false);
  const [start, other] = (await find(stack, name)).current_version.config.steps;
  expect(start.config.failover?.on).toContain("client_error");
  expect(other.config.tts?.clean_text).toBeUndefined();
  expect(other.config.failover).toBeUndefined();

  await page.reload();
  await connectAdmin(page, stack);
  await openAgent(page, name);
  await openAgentSettings(page);
  await expect(page.locator("details.advanced")).toHaveAttribute("open", "");
  await expect(page.getByLabel(/Clean text before speaking/)).not.toBeChecked();
  await expect(page.getByLabel("Rejected requests (bad request, bad key)", { exact: true })).toBeChecked();
});

test("interruption settings are published, read back and shown after a reload", async ({ page, stack }) => {
  const name = "E2E Interruptions";
  await api(stack, "/agents", {
    method: "POST",
    body: JSON.stringify({ name, config: { stt: stage({ model: "chirp" }), llm: stage({ model: "m", system_prompt: "You help." }), tts: stage({ voice: "v" }) } }),
  });
  const turnOf = async () => ((await api<{ data: { name: string; current_version: { config: { turn?: object } } }[] }>(stack, "/agents")).data.find((f) => f.name === name)!).current_version.config.turn;
  await connectAdmin(page, stack);
  await openAgent(page, name);
  await openAgentSettings(page);
  await page.locator("details.advanced").getByText("Advanced", { exact: true }).click();
  await page.getByRole("radio", { name: "After a number of words" }).check();
  await page.getByRole("spinbutton", { name: "Words before the caller interrupts" }).fill("3");
  await page.getByRole("checkbox", { name: "Not while a tool runs" }).check();
  await page.getByRole("spinbutton", { name: "Longest mute (seconds)" }).fill("20");
  await page.getByRole("button", { name: "Done" }).click();
  await page.getByRole("button", { name: "Publish", exact: true }).click();
  await page.getByRole("button", { name: "Publish v2" }).click();

  await expect.poll(turnOf).toEqual({ min_words: 3, mute: ["function_call"], max_mute_ms: 20000 });

  await page.reload();
  await connectAdmin(page, stack);
  await openAgent(page, name);
  await openAgentSettings(page);
  await expect(page.locator("details.advanced")).toHaveAttribute("open", "");
  await expect(page.getByRole("radio", { name: "After a number of words" })).toBeChecked();
  await expect(page.getByRole("spinbutton", { name: "Words before the caller interrupts" })).toHaveValue("3");
  await expect(page.getByRole("checkbox", { name: "Not while a tool runs" })).toBeChecked();
  await expect(page.getByRole("spinbutton", { name: "Longest mute (seconds)" })).toHaveValue("20");
});

test("end-of-turn settings are published, read back and shown after a reload", async ({ page, stack }) => {
  const name = "E2E End Of Turn";
  await api(stack, "/agents", {
    method: "POST",
    body: JSON.stringify({ name, config: { stt: stage({ model: "chirp" }), llm: stage({ model: "m", system_prompt: "You help." }), tts: stage({ voice: "v" }) } }),
  });
  const turnOf = async () => ((await api<{ data: { name: string; current_version: { config: { turn?: object } } }[] }>(stack, "/agents")).data.find((f) => f.name === name)!).current_version.config.turn;
  await connectAdmin(page, stack);
  await openAgent(page, name);
  await openAgentSettings(page);
  await page.locator("details.advanced").getByText("Advanced", { exact: true }).click();
  const detection = page.getByRole("combobox", { name: "End-of-turn detection" });
  // The echo stack loads no Smart Turn model: the choice is visible but cannot be picked.
  await expect(detection.getByRole("option", { name: /^Smart: .*\(not available on this server\)$/ })).toHaveJSProperty("disabled", true);
  await detection.selectOption({ label: "Fast: end on a full sentence" });
  await page.getByRole("spinbutton", { name: "Wait for the caller to resume (ms)" }).fill("900");
  await page.getByRole("button", { name: "Done" }).click();
  await page.getByRole("button", { name: "Publish", exact: true }).click();
  await page.getByRole("button", { name: "Publish v2" }).click();

  await expect.poll(turnOf).toEqual({ speech_timeout_ms: 900, end_of_turn: "eager" });

  await page.reload();
  await connectAdmin(page, stack);
  await openAgent(page, name);
  await openAgentSettings(page);
  await expect(page.locator("details.advanced")).toHaveAttribute("open", "");
  await expect(page.getByRole("combobox", { name: "End-of-turn detection" })).toHaveValue("eager");
  await expect(page.getByRole("spinbutton", { name: "Wait for the caller to resume (ms)" })).toHaveValue("900");
});

test("a step's background sound is published, read back and shown after a reload", async ({ page, stack }) => {
  const name = "E2E Step Background";
  const cfg = (extra: object = {}) => ({ llm: stage({ model: "m", system_prompt: "You help." }), tts: stage({ voice: "v" }), ...extra });
  await api(stack, "/agents", {
    method: "POST",
    body: JSON.stringify({
      name,
      config: {
        start: "front",
        steps: [
          { id: "front", name: "Front", config: { stt: stage({ model: "chirp" }), ...cfg() } },
          { id: "sales", name: "Sales", config: cfg() },
        ],
        handoffs: [{ id: "e1", from: "front", to: "sales", condition: "wants to buy" }],
      },
    }),
  });
  type Listed = { name: string; current_version: { config: { steps: { id: string; config: { audio?: object } }[] } } };
  const salesAudio = async () => {
    const agents = (await api<{ data: Listed[] }>(stack, "/agents")).data;
    return agents.find((f) => f.name === name)!.current_version.config.steps.find((s) => s.id === "sales")!.config.audio;
  };
  await connectAdmin(page, stack);
  await openAgent(page, name);
  await stepCard(page, "Sales").click();
  await page.getByRole("tab", { name: /Advanced/ }).click();
  await expect(page.getByRole("combobox", { name: "Background sound" })).toHaveValue("");
  await page.getByRole("combobox", { name: "Background sound" }).selectOption("office");
  await page.getByRole("button", { name: "Publish", exact: true }).click();
  await page.getByRole("button", { name: "Publish v2" }).click();

  await expect.poll(salesAudio).toEqual({ background: { name: "office" } });

  await page.reload();
  await connectAdmin(page, stack);
  await openAgent(page, name);
  await stepCard(page, "Sales").click();
  await page.getByRole("tab", { name: /Advanced/ }).click();
  await expect(page.getByRole("combobox", { name: "Background sound" })).toHaveValue("office");
});

test("a hold sound is published, read back and shown after a reload", async ({ page, stack }) => {
  const name = "E2E Hold Sound";
  await api(stack, "/agents", {
    method: "POST",
    body: JSON.stringify({ name, config: { stt: stage({ model: "chirp" }), llm: stage({ model: "m", system_prompt: "You help." }), tts: stage({ voice: "v" }) } }),
  });
  type Listed = { name: string; current_version: { config: { audio?: object } } };
  const audioOf = async () => ((await api<{ data: Listed[] }>(stack, "/agents")).data.find((f) => f.name === name)!).current_version.config.audio;
  await connectAdmin(page, stack);
  await openAgent(page, name);
  await openAgentSettings(page);
  await page.locator("details.advanced").getByText("Advanced", { exact: true }).click();
  await page.getByRole("combobox", { name: "Hold sound" }).selectOption("chime");
  await page.getByRole("spinbutton", { name: "Start the hold sound after (ms)" }).fill("1000");
  await page.getByRole("button", { name: "Done" }).click();
  await page.getByRole("button", { name: "Publish", exact: true }).click();
  await page.getByRole("button", { name: "Publish v2" }).click();

  await expect.poll(audioOf).toEqual({ tool_cue: { name: "chime", after_ms: 1000 } });

  await page.reload();
  await connectAdmin(page, stack);
  await openAgent(page, name);
  await openAgentSettings(page);
  await expect(page.locator("details.advanced")).toHaveAttribute("open", "");
  await expect(page.getByRole("combobox", { name: "Hold sound" })).toHaveValue("chime");
  await expect(page.getByRole("spinbutton", { name: "Start the hold sound after (ms)" })).toHaveValue("1000");
});

test("a one-step agent can end the call: published in steps form, read back and shown after a reload", async ({ page, stack }) => {
  const name = "E2E One Step End Call";
  await api(stack, "/agents", {
    method: "POST",
    body: JSON.stringify({ name, config: { stt: stage({ model: "chirp" }), llm: stage({ model: "m", system_prompt: "You help." }), tts: stage({ voice: "v" }) } }),
  });
  type Listed = { name: string; current_version: { config: { steps?: { end_call?: boolean }[] } } };
  const endCallOf = async () => ((await api<{ data: Listed[] }>(stack, "/agents")).data.find((f) => f.name === name)!).current_version.config.steps?.[0]?.end_call;
  await connectAdmin(page, stack);
  await openAgent(page, name);
  await page.locator(".react-flow__node").first().click();
  await page.getByRole("tab", { name: /Tools/ }).click();
  await page.getByRole("checkbox", { name: /can end the call/i }).check();
  await page.getByRole("button", { name: "Publish", exact: true }).click();
  await page.getByRole("button", { name: "Publish v2" }).click();

  await expect.poll(endCallOf).toBe(true);

  await page.reload();
  await connectAdmin(page, stack);
  await openAgent(page, name);
  await page.locator(".react-flow__node").first().click();
  await page.getByRole("tab", { name: /Tools/ }).click();
  await expect(page.getByRole("checkbox", { name: /can end the call/i })).toBeChecked();
});

test("a shorter call limit is published in seconds, read back and shown in minutes after a reload", async ({ page, stack }) => {
  const name = "E2E Call Limit";
  await api(stack, "/agents", {
    method: "POST",
    body: JSON.stringify({ name, config: { stt: stage({ model: "chirp" }), llm: stage({ model: "m", system_prompt: "You help." }), tts: stage({ voice: "v" }) } }),
  });
  type Listed = { name: string; current_version: { config: { max_call_duration_seconds?: number } } };
  const limitOf = async () => ((await api<{ data: Listed[] }>(stack, "/agents")).data.find((f) => f.name === name)!).current_version.config.max_call_duration_seconds;
  await connectAdmin(page, stack);
  await openAgent(page, name);
  await openAgentSettings(page);
  await page.locator("details.advanced").getByText("Advanced", { exact: true }).click();
  await page.getByRole("spinbutton", { name: "Longest call (minutes)" }).fill("10");
  await page.getByRole("button", { name: "Done" }).click();
  await page.getByRole("button", { name: "Publish", exact: true }).click();
  await page.getByRole("button", { name: "Publish v2" }).click();

  await expect.poll(limitOf).toBe(600);

  await page.reload();
  await connectAdmin(page, stack);
  await openAgent(page, name);
  await openAgentSettings(page);
  await expect(page.locator("details.advanced")).toHaveAttribute("open", "");
  await expect(page.getByRole("spinbutton", { name: "Longest call (minutes)" })).toHaveValue("10");
});
