import type { Page } from "@playwright/test";
import type { Stack } from "./agents-stack";
import { expect } from "@playwright/test";
import { api, botReplies, botReply, sendText } from "./agents-helpers";
import { pace } from "./mcp-helpers";

type Stored = {
  id: string;
  current_version: { config: { start: string; steps: any[]; handoffs: any[] }; mcp_tools: any[] };
};

// The seeded dental agent uses xai/elevenlabs stages, which cannot take an echo call. This creates
// "Dental (echo)": the same steps, handoffs, end_call flags, step tools and MCP selections, with
// every stage on the echo provider.
export async function createEchoDentalVariant(stack: Stack, installedId: string) {
  const { data } = await api<{ data: Stored }>(stack, `/agents/${installedId}`);
  const { config, mcp_tools } = data.current_version;
  const steps = config.steps.map((s) => {
    const c: any = {};
    if (s.config.stt) c.stt = { provider: "echo", model: "chirp_2" };
    c.llm = { provider: "echo", model: "gemini-2.5-flash", system_prompt: s.config.llm.system_prompt };
    c.tts = { provider: "echo", voice: "en-US-Chirp3-HD-Aoede" };
    return { ...s, config: c };
  });
  await api(stack, "/agents", {
    method: "POST",
    body: JSON.stringify({ name: "Dental (echo)", config: { ...config, steps }, mcp_tools }),
  });
  return { config, mcp_tools };
}

// Sends a phrase the fake LLM obeys and waits for a NEW bot reply (the count of final replies
// rises, and with `reply` one of them matches), then paces the next turn.
export async function turn(page: Page, text: string, reply?: RegExp) {
  const before = await botReplies(page).count();
  await sendText(page, text);
  await expect.poll(() => botReplies(page).count(), { timeout: 20_000 }).toBeGreaterThan(before);
  if (reply) await botReply(page, reply).waitFor({ timeout: 20_000 });
  await pace(page);
}

export async function toolTurn(page: Page, tool: string, args: object, reply: RegExp = new RegExp(`Tool ${tool} returned`)) {
  await turn(page, `call tool ${tool} ${JSON.stringify(args)}`, reply);
}
