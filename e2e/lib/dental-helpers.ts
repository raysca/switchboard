import type { Page } from "@playwright/test";
import type { Stack } from "./flows-stack";
import { api, botReply, sendText } from "./flows-helpers";
import { pace } from "./mcp-helpers";

type Stored = {
  id: string;
  current_version: { config: { start: string; steps: any[]; routes: any[] }; mcp_tools: any[] };
};

// The installed dental flow uses xai/elevenlabs stages, which have no fake build. This creates
// "Dental (fake)": the same steps, routes, end_call flags, step tools and MCP selections, with
// every stage on the google fake provider.
export async function createFakeDentalVariant(stack: Stack, installedId: string) {
  const { data } = await api<{ data: Stored }>(stack, `/flows/${installedId}`);
  const { config, mcp_tools } = data.current_version;
  const steps = config.steps.map((s) => {
    const c: any = {};
    if (s.config.stt) c.stt = { provider: "google", model: "chirp" };
    c.llm = { provider: "google", model: "fake-llm", system_prompt: s.config.llm.system_prompt };
    c.tts = { provider: "google", voice: "v" };
    return { ...s, config: c };
  });
  await api(stack, "/flows", {
    method: "POST",
    body: JSON.stringify({ name: "Dental (fake)", config: { ...config, steps }, mcp_tools }),
  });
  return { config, mcp_tools };
}

// Sends a phrase the fake LLM obeys and waits for the bot reply, pacing the next turn.
export async function turn(page: Page, text: string, reply: RegExp) {
  await sendText(page, text);
  await botReply(page, reply).waitFor({ timeout: 20_000 });
  await pace(page);
}

export async function toolTurn(page: Page, tool: string, args: object, reply: RegExp = new RegExp(`Tool ${tool} returned`)) {
  await turn(page, `call tool ${tool} ${JSON.stringify(args)}`, reply);
}
