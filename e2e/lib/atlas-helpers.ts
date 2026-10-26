import type { Page } from "@playwright/test";
import { expect, type Stack } from "./agents-stack";
import { api, connectAdmin, stepCard } from "./agents-helpers";

// The Atlas demo on the free echo provider: the Dashboard install, the stored agent the
// installer publishes, and the echo variant the call test drives (the installed stages name
// providers the fake stack cannot build). The demo's own card is a labelled region and its
// button is distinct, so every Atlas lookup is scoped to it and can never hit the dental card.

export const ATLAS_AGENT = "Atlas Travel Concierge";
export const ATLAS_ECHO_AGENT = "Atlas (echo)";

export type AtlasPin = { mcp_server_id: string; remote_name: string; schema_hash: string; required: boolean };
export type AtlasHandoff = { id: string; from: string; to: string; condition: string };
export type AtlasStep = {
  id: string;
  name: string;
  end_call?: boolean;
  tools?: string[];
  position?: { x: number; y: number };
  config: {
    stt?: Record<string, unknown>;
    llm: Record<string, unknown>;
    tts: Record<string, unknown>;
    async_tools?: string[];
    [key: string]: unknown;
  };
};
export type AtlasAgent = {
  id: string;
  name: string;
  current_version: {
    config: { start: string; steps: AtlasStep[]; handoffs: AtlasHandoff[] };
    mcp_tools: AtlasPin[];
  };
};

/** The Dashboard's Atlas card: a labelled region, so its controls are never ambiguous. */
export const atlasDemoPanel = (page: Page) => page.getByRole("region", { name: "Atlas Travel demo" });

/** Loads the demo from the Dashboard and waits for the installed agent link in the same card. */
export async function loadAtlasDemo(page: Page, stack: Stack) {
  await connectAdmin(page, stack);
  const panel = atlasDemoPanel(page);
  await expect(panel).toBeVisible();
  await expect(panel).toHaveAttribute("data-demo", "atlas");
  await panel.getByRole("button", { name: "Load Atlas demo", exact: true }).click();
  // The install swaps the button for a link to the published agent; discovery plus the two HTTP
  // tool creates can take a moment, so give it room.
  await expect(panel.getByRole("link", { name: ATLAS_AGENT, exact: true })).toBeVisible({ timeout: 30_000 });
}

/** The stored agent the installer published, by name through the management API. */
export async function storedAtlasAgent(stack: Stack): Promise<AtlasAgent> {
  const agents = await api<{ data: { id: string; name: string }[] }>(stack, "/agents");
  const installed = agents.data.find((a) => a.name === ATLAS_AGENT);
  expect(installed, `the demo installs the ${ATLAS_AGENT} agent`).toBeTruthy();
  const { data } = await api<{ data: AtlasAgent }>(stack, `/agents/${installed!.id}`);
  return data;
}

// Every installed stage names google, xai or elevenlabs; the fake stack builds none of those.
// "Atlas (echo)" keeps the steps, handoffs, end_call flags, per-step tools and the full tool
// selection (including the two http: pins) and swaps only the stages for the echo provider. The
// system prompt becomes the step name: the echo model prefixes each reply with it, so a transcript
// reads by step. The start step keeps stt, context, the call cap and async_tools; every other step
// keeps its greeting and max_parallel.
export async function createEchoAtlasVariant(stack: Stack, installedId: string) {
  const { data } = await api<{ data: AtlasAgent }>(stack, `/agents/${installedId}`);
  const { config, mcp_tools } = data.current_version;
  const steps = config.steps.map((s) => {
    const { stt, llm, tts, ...rest } = s.config;
    const stepConfig: Record<string, unknown> = { ...rest };
    if (stt) stepConfig.stt = { ...stt, provider: "echo", model: "chirp_2" };
    stepConfig.llm = { ...llm, provider: "echo", model: "gemini-2.5-flash", system_prompt: s.name };
    stepConfig.tts = { ...tts, provider: "echo", voice: "en-US-Chirp3-HD-Aoede" };
    return { ...s, config: stepConfig };
  });
  const created = await api<{ data: { id: string; name: string } }>(stack, "/agents", {
    method: "POST",
    body: JSON.stringify({ name: ATLAS_ECHO_AGENT, config: { ...config, steps }, mcp_tools }),
  });
  return { id: created.data.id, config: { ...config, steps }, mcp_tools };
}

/** Waits for a step to become the active step of the live call. */
export async function expectActiveStep(page: Page, name: string) {
  await expect(stepCard(page, name).locator(".step-node")).toHaveAttribute("data-state", "active", { timeout: 15_000 });
}

// The phrase helpers are the dental suite's: send a phrase the fake model obeys, wait for a new
// reply, then pace the next turn.
export { toolTurn, turn } from "./dental-helpers";
