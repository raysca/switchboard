import { test, expect, LIVE } from "../lib/agents-stack";
import { step } from "../lib/evidence";
import { api, botReply, connectAdmin, openAgentTest, sendText, stage, startCall } from "../lib/agents-helpers";

// Per-turn latency is stored and shown in Call detail; a provider is checked only when asked.
test.skip(LIVE, "the labelled echo reply and the instant echo provider are echo-only");
test.describe.configure({ mode: "serial" });

const NAME = "E2E Latency";

test("a finished call shows how long the reply took", async ({ page, stack }) => {
  await api(stack, "/agents", {
    method: "POST",
    body: JSON.stringify({ name: NAME, config: { stt: stage({ model: "chirp" }), llm: stage({ model: "m", system_prompt: "LATENCY PROMPT" }), tts: stage({ voice: "frontvoice" }) } }),
  });
  await connectAdmin(page, stack);
  await openAgentTest(page, NAME);
  await startCall(page);
  await sendText(page, "hello");
  await expect(botReply(page, /LATENCY PROMPT/)).toBeVisible();
  const sessionId = (await page.getByTestId("session-summary").locator("code").textContent())!.trim();
  await page.getByTestId("disconnect-btn").click();

  // The stored turn carries its stage latencies (a typed turn: model and voice, no end of speech).
  await expect
    .poll(async () => {
      const turns = (await api<{ data: { latencies?: Record<string, number> }[] }>(stack, `/sessions/${sessionId}/turns`)).data;
      return turns[0]?.latencies && "llm_ttft_ms" in turns[0].latencies && "tts_ttfb_ms" in turns[0].latencies;
    })
    .toBeTruthy();

  await page.getByTestId("view-trace-link").click(); // in-app: a reload would drop the in-memory key
  await expect(page.getByText("Replied in")).toBeVisible();
  await expect(page.getByText("Median reply")).toBeVisible();
  await step(page, "call detail shows the reply time");
});

test("a provider is checked only when the button is pressed", async ({ page, stack }) => {
  const asked: string[] = [];
  page.on("request", (r) => {
    if (r.url().includes("/providers/") && r.url().endsWith("/test")) asked.push(r.method());
  });
  await connectAdmin(page, stack);
  await page.getByRole("link", { name: "Providers", exact: true }).first().click();
  await page.getByRole("button", { name: /Echo/ }).click();
  await expect(page.getByRole("heading", { name: /Echo/, level: 2 })).toBeVisible();
  await page.waitForTimeout(500);
  expect(asked).toEqual([]); // nothing automatic

  await page.getByRole("button", { name: "Test connection" }).click();
  const results = page.getByRole("list", { name: "Connection test results" });
  await expect(results.getByRole("listitem")).toHaveCount(3);
  await expect(results.getByRole("listitem").filter({ hasText: "OK" })).toHaveCount(3);
  await step(page, "provider test results");

  // A second press inside the limit is turned away with the server's message.
  await page.getByRole("button", { name: "Test connection" }).click();
  await expect(page.getByRole("alert")).toContainText("try again in");
  expect(asked).toEqual(["POST", "POST"]);
});
