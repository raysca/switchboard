import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { ProvidersView } from "./ProvidersView";
import type { ApiClient, ProviderStatus } from "../../api/client";

function renderView(client: ApiClient, keyValue: string | null = "key-1") {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <ProvidersView client={client} keyValue={keyValue} />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const google: ProviderStatus = {
  id: "google", display_name: "Google", capabilities: ["stt", "llm"], detected: true, env: ["GOOGLE_API_KEY"],
  services: {
    stt: { available: true, custom: true },
    llm: { available: true, custom: true },
    tts: { available: false, reason: "not_in_build", custom: true },
  },
};
const xai: ProviderStatus = {
  id: "xai", display_name: "xAI", capabilities: [], detected: false, env: ["XAI_API_KEY"],
  services: { llm: { available: false, reason: "missing_env", missing_env: ["XAI_API_KEY"], custom: true } },
};

const clientWith = (providers: ProviderStatus[]) =>
  ({ getProviders: vi.fn().mockResolvedValue(providers) }) as unknown as ApiClient;

describe("ProvidersView", () => {
  it("prompts for a management key when absent", () => {
    renderView(clientWith([]), null);
    expect(screen.getByText(/enter a management key/i)).toBeVisible();
  });

  it("renders a row per provider with detection, unlocked services and missing variables", async () => {
    renderView(clientWith([google, xai]));
    const gRow = (await screen.findByRole("cell", { name: /Google/ })).closest("tr")!;
    expect(within(gRow).getByText("Detected")).toBeVisible();
    expect(within(gRow).getByText(/stt, llm/i)).toBeVisible();
    expect(within(gRow).getByText(/tts not in this build/i)).toBeVisible();
    const xRow = screen.getByRole("cell", { name: /xAI/ }).closest("tr")!;
    expect(within(xRow).getByText("Not detected")).toBeVisible();
    expect(within(xRow).getByText(/XAI_API_KEY/)).toBeVisible();
  });

  it("is read-only: no inputs and no buttons", async () => {
    renderView(clientWith([google, xai]));
    await screen.findByRole("cell", { name: /Google/ });
    expect(screen.queryAllByRole("textbox")).toHaveLength(0);
    expect(document.querySelector("input, select, textarea, form")).toBeNull();
    expect(screen.queryAllByRole("button")).toHaveLength(0);
  });

  it("names the variables to set when nothing is detected", async () => {
    renderView(clientWith([{ ...xai }, { ...google, detected: false, capabilities: [], env: ["GOOGLE_API_KEY"] }]));
    expect(await screen.findByText(/no providers detected/i)).toBeVisible();
    expect(screen.getAllByText(/XAI_API_KEY/).length).toBeGreaterThan(0);
    expect(screen.getAllByText(/GOOGLE_API_KEY/).length).toBeGreaterThan(0);
  });
});
