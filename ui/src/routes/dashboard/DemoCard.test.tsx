import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import type { ApiClient } from "../../api/client";
import { DemoCard } from "./DemoCard";

function renderWithProviders(ui: React.ReactElement) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>{ui}</MemoryRouter>
    </QueryClientProvider>,
  );
}

const dentalStatus = { available: true, installed: false };
const atlasStatus = { available: true, installed: false };
const unavailable = { available: false, installed: false };

describe("DemoCard", () => {
  it("offers nothing when the server has no demo endpoint", async () => {
    // An older server/mocked response has no atlas object at all.
    const client = { getDemo: vi.fn().mockResolvedValue({ dental: unavailable }) } as unknown as ApiClient;
    renderWithProviders(<DemoCard client={client} />);
    await waitFor(() => expect(client.getDemo).toHaveBeenCalled());
    expect(screen.queryByRole("region")).toBeNull();
    expect(screen.queryByRole("button", { name: /load demo/i })).toBeNull();
  });

  it("renders only the dental panel when only the dental demo is available", async () => {
    const client = {
      getDemo: vi.fn().mockResolvedValue({ dental: dentalStatus, atlas: unavailable }),
    } as unknown as ApiClient;
    renderWithProviders(<DemoCard client={client} />);
    expect(await screen.findByRole("heading", { name: "Dentistry demo" })).toBeVisible();
    expect(screen.getByRole("region", { name: "Dentistry demo" })).toHaveAttribute("data-demo", "dental");
    expect(screen.queryByRole("region", { name: "Atlas Travel demo" })).toBeNull();
    expect(screen.getByText(/scheduling tool server/i)).toBeVisible();
  });

  it("renders only the Atlas panel when only the Atlas demo is available", async () => {
    const client = {
      getDemo: vi.fn().mockResolvedValue({ dental: unavailable, atlas: atlasStatus }),
    } as unknown as ApiClient;
    renderWithProviders(<DemoCard client={client} />);
    expect(await screen.findByRole("heading", { name: "Atlas Travel demo" })).toBeVisible();
    const panel = screen.getByRole("region", { name: "Atlas Travel demo" });
    expect(panel).toHaveAttribute("data-demo", "atlas");
    expect(within(panel).getByText(/8 steps/)).toBeVisible();
    expect(within(panel).getByText(/32 handoffs/)).toBeVisible();
    expect(within(panel).getByText(/travel tool server and two HTTP tools/i)).toBeVisible();
    expect(within(panel).getByRole("button", { name: "Load Atlas demo" })).toBeEnabled();
    expect(screen.queryByRole("region", { name: "Dentistry demo" })).toBeNull();
  });

  it("renders both panels with independent install buttons when both demos are available", async () => {
    const installDentalDemo = vi.fn().mockResolvedValue({ mcp_server_id: "m", mcp_created: true, tools: 7, agent_id: "f1", agent_created: true });
    const installAtlasDemo = vi.fn().mockResolvedValue({ mcp_server_id: "atlas", mcp_created: true, tools: 18, http_tools: 2, agent_id: "a1", agent_created: true });
    const client = {
      getDemo: vi.fn().mockResolvedValue({ dental: dentalStatus, atlas: atlasStatus }),
      installDentalDemo,
      installAtlasDemo,
    } as unknown as ApiClient;
    renderWithProviders(<DemoCard client={client} />);

    const dentalPanel = await screen.findByRole("region", { name: "Dentistry demo" });
    const atlasPanel = screen.getByRole("region", { name: "Atlas Travel demo" });
    // The dental button keeps the name the dental browser test clicks without scoping.
    await userEvent.click(within(dentalPanel).getByRole("button", { name: "Load demo" }));
    expect(installDentalDemo).toHaveBeenCalledTimes(1);
    expect(installAtlasDemo).not.toHaveBeenCalled();

    await userEvent.click(within(atlasPanel).getByRole("button", { name: "Load Atlas demo" }));
    expect(installAtlasDemo).toHaveBeenCalledTimes(1);
    expect(installDentalDemo).toHaveBeenCalledTimes(1);
  });

  it("installs the dental demo and links to the agent", async () => {
    const getDemo = vi
      .fn()
      .mockResolvedValueOnce({ dental: { available: true, installed: false } })
      .mockResolvedValue({ dental: { available: true, installed: true, agent_id: "f1" } });
    const installDentalDemo = vi.fn().mockResolvedValue({ mcp_server_id: "m", mcp_created: true, tools: 7, agent_id: "f1", agent_created: true });
    renderWithProviders(<DemoCard client={{ getDemo, installDentalDemo } as unknown as ApiClient} />);
    await userEvent.click(await screen.findByRole("button", { name: /load demo/i }));
    expect(installDentalDemo).toHaveBeenCalledTimes(1);
    expect(await screen.findByRole("link", { name: /smile dental front desk/i })).toHaveAttribute("href", "/agents/f1");
    // The installer can be run again to repair a stale pin; the dental button stays distinct.
    expect(screen.getByRole("button", { name: "Reload demo" })).toBeEnabled();
  });

  it("installs the Atlas demo from its own panel and links to the Atlas Travel Concierge agent", async () => {
    const getDemo = vi
      .fn()
      .mockResolvedValueOnce({ dental: dentalStatus, atlas: atlasStatus })
      .mockResolvedValue({ dental: dentalStatus, atlas: { available: true, installed: true, agent_id: "atlas-f1" } });
    const installDentalDemo = vi.fn();
    const installAtlasDemo = vi.fn().mockResolvedValue({ mcp_server_id: "atlas", mcp_created: true, tools: 18, http_tools: 2, agent_id: "atlas-f1", agent_created: true });
    renderWithProviders(<DemoCard client={{ getDemo, installDentalDemo, installAtlasDemo } as unknown as ApiClient} />);

    const atlasPanel = await screen.findByRole("region", { name: "Atlas Travel demo" });
    await userEvent.click(within(atlasPanel).getByRole("button", { name: "Load Atlas demo" }));

    expect(installAtlasDemo).toHaveBeenCalledTimes(1);
    expect(installDentalDemo).not.toHaveBeenCalled();
    expect(await screen.findByRole("link", { name: /atlas travel concierge/i })).toHaveAttribute("href", "/agents/atlas-f1");
    // The dental panel is unaffected: it still offers its own install.
    expect(within(screen.getByRole("region", { name: "Dentistry demo" })).getByRole("button", { name: /load demo/i })).toBeEnabled();
  });

  it("shows the Atlas failure in its own panel and lets the user retry", async () => {
    const getDemo = vi
      .fn()
      .mockResolvedValueOnce({ dental: dentalStatus, atlas: atlasStatus })
      .mockResolvedValue({ dental: dentalStatus, atlas: { available: true, installed: true, agent_id: "atlas-f1" } });
    const installAtlasDemo = vi
      .fn()
      .mockRejectedValueOnce(new Error("boom"))
      .mockResolvedValue({ mcp_server_id: "atlas", mcp_created: true, tools: 18, http_tools: 2, agent_id: "atlas-f1", agent_created: true });
    renderWithProviders(<DemoCard client={{ getDemo, installDentalDemo: vi.fn(), installAtlasDemo } as unknown as ApiClient} />);

    const atlasPanel = await screen.findByRole("region", { name: "Atlas Travel demo" });
    const dentalPanel = screen.getByRole("region", { name: "Dentistry demo" });
    await userEvent.click(within(atlasPanel).getByRole("button", { name: "Load Atlas demo" }));

    expect(await within(atlasPanel).findByRole("alert")).toHaveTextContent(/could not be installed/i);
    expect(within(dentalPanel).queryByRole("alert")).toBeNull();
    expect(within(atlasPanel).getByRole("button", { name: "Load Atlas demo" })).toBeEnabled();

    await userEvent.click(within(atlasPanel).getByRole("button", { name: "Load Atlas demo" }));
    expect(installAtlasDemo).toHaveBeenCalledTimes(2);
    expect(await within(atlasPanel).findByRole("link", { name: /atlas travel concierge/i })).toHaveAttribute("href", "/agents/atlas-f1");
  });

  it("shows the failure and lets the user retry", async () => {
    const installDentalDemo = vi.fn().mockRejectedValue(new Error("boom"));
    const client = {
      getDemo: vi.fn().mockResolvedValue({ dental: { available: true, installed: false } }),
      installDentalDemo,
    } as unknown as ApiClient;
    renderWithProviders(<DemoCard client={client} />);
    await userEvent.click(await screen.findByRole("button", { name: /load demo/i }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/could not be installed/i);
    expect(screen.getByRole("button", { name: /load demo/i })).toBeEnabled();
  });
});
