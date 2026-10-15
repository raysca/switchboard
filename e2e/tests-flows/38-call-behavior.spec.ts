import { test, expect, LIVE } from "../lib/flows-stack";
import { step } from "../lib/evidence";
import { api, connectAdmin, openFlow, openFlowSettings, openFlowsList, startCall, stage } from "../lib/flows-helpers";

// Per-flow quiet-caller and audio settings: set in Flow settings, tried on a draft test call before
// publishing, published, read back from the server and reopened. Echo providers: free and deterministic.
// The suite's server has quiet-caller handling off, so what ends the call here is this flow's own setting.
test.skip(LIVE, "echo-provider spec");
test.describe.configure({ mode: "serial" });

const NAME = "E2E Call Behavior";
const PROMPT = "Still there for the test?";
const BYE = "Goodbye for now.";
type Config = { idle?: { user_timeout_ms?: number; max_prompts?: number; prompt?: string; prompt_mode?: string; goodbye?: string }; audio?: { denoise?: object; output_gain_db?: number; background?: { name: string; gain_db?: number; loop?: boolean } } };
type Flow = { name: string; current_version: { number: number; config: Config & { steps?: { config: Config }[] } } };
const current = async (stack: Parameters<typeof api>[0]) => (await api<{ data: Flow[] }>(stack, "/flows")).data.find((f) => f.name === NAME)!.current_version;
const startConfig = (c: Flow["current_version"]["config"]): Config => c.steps?.[0]?.config ?? c;

test("quiet-caller and audio settings are tried on a draft call, published, read back and reopened", async ({ page, stack }) => {
  await api(stack, "/flows", {
    method: "POST",
    body: JSON.stringify({ name: NAME, config: { stt: stage({ model: "chirp" }), llm: stage({ model: "m", system_prompt: "You help." }), tts: stage({ voice: "v" }) } }),
  });
  await connectAdmin(page, stack);
  await openFlow(page, NAME);

  // Basic: the checkbox is off because the server has the handling off; ticking it asks for the built-in wait.
  await openFlowSettings(page);
  const tick = page.getByLabel("End the call if the caller stays silent", { exact: true });
  await expect(tick).not.toBeChecked();
  // The server answers with its own defaults: handling off, so there is no wait to suggest.
  await expect(page.getByLabel("Wait this long (seconds)", { exact: true })).toHaveAttribute("placeholder", "Server default");
  await tick.check();
  await page.getByLabel("Wait this long (seconds)", { exact: true }).fill("2");

  // Advanced: how many times, exactly what to say, the goodbye, then audio.
  await page.locator("details.advanced").getByText("Advanced", { exact: true }).click();
  await page.getByLabel("Ask this many times", { exact: true }).fill("1");
  await page.locator("#flow-idle-prompt").fill(PROMPT);
  await page.getByLabel("Say it exactly", { exact: true }).check();
  await page.getByLabel("Goodbye before hanging up", { exact: true }).fill(BYE);
  await page.getByLabel("Reduce background noise", { exact: true }).check();
  await page.getByLabel("Voice volume (dB)", { exact: true }).fill("-3");
  const sound = page.getByRole("combobox", { name: "Background sound" });
  expect(await sound.locator("option").allTextContents()).toEqual(["None", "office"]); // the server's own sounds
  await sound.selectOption("office");
  await page.getByLabel("Background volume (dB)", { exact: true }).fill("-25");
  await page.getByLabel("Repeat", { exact: true }).uncheck();
  await page.getByRole("button", { name: "Explain Voice volume" }).click();
  await expect(page.getByRole("region", { name: "Explain Voice volume" })).toContainText("Default and range");
  await page.keyboard.press("Escape");
  await expect(page.getByRole("region", { name: "Explain Voice volume" })).toHaveCount(0);
  await step(page, "quiet-caller and audio settings");
  await page.getByRole("button", { name: "Done" }).click();

  // A draft test call, before anything is published: nobody speaks, so the agent asks and then says goodbye.
  await page.getByRole("button", { name: "Test call" }).click();
  await expect(page.getByRole("note").filter({ hasText: "This call runs your unpublished changes" })).toBeVisible();
  await startCall(page);
  // The question and goodbye are spoken, not written in the transcript (the Go e2e pins their exact text);
  // what the console shows is the hang-up, which only this flow's wait can cause on this server.
  await expect(page.getByText("user_idle")).toBeVisible({ timeout: 30_000 });
  await expect(page.getByTestId("connection-state")).toHaveText(/disconnected/i);
  await step(page, "the draft call asked and hung up");

  // Publish and read the version back.
  await page.getByRole("button", { name: "Publish", exact: true }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toContainText("Flow settings: silent-caller behavior changed");
  await expect(dialog).toContainText("Flow settings: audio changed");
  await page.getByRole("button", { name: "Publish v2" }).click();
  await expect.poll(async () => (await current(stack)).number).toBe(2);
  const cfg = startConfig((await current(stack)).config);
  expect(cfg.idle).toEqual({ user_timeout_ms: 2000, max_prompts: 1, prompt: PROMPT, prompt_mode: "say", goodbye: BYE });
  expect(cfg.audio).toEqual({ denoise: {}, output_gain_db: -3, background: { name: "office", gain_db: -25, loop: false } });

  // Reopened: Advanced is open with the stored values, and a too-short wait blocks publishing with the reason.
  await openFlowsList(page);
  await openFlow(page, NAME);
  await openFlowSettings(page);
  await expect(page.locator("details.advanced")).toHaveAttribute("open", "");
  await expect(page.locator("#flow-idle-prompt")).toHaveValue(PROMPT);
  await expect(page.getByRole("combobox", { name: "Background sound" })).toHaveValue("office");
  await expect(page.getByLabel("Background volume (dB)", { exact: true })).toHaveValue("-25");
  await expect(page.getByLabel("Repeat", { exact: true })).not.toBeChecked();
  await page.getByLabel("Wait this long (seconds)", { exact: true }).fill("0.5");
  await page.getByRole("button", { name: "Done" }).click();
  await page.getByRole("button", { name: "Publish", exact: true }).click();
  await expect(page.getByRole("dialog")).toContainText("Wait for a silent caller must be 1 to 300 seconds, or 0 to never hang up.");
  await expect(page.getByRole("button", { name: "Publish v3" })).toBeDisabled();
  await step(page, "a too-short wait blocks publishing");
});
