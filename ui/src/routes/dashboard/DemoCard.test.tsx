import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
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

describe("DemoCard", () => {
  it("offers the dental demo only when the server has an MCP endpoint for it", async () => {
    const client = { getDemo: vi.fn().mockResolvedValue({ dental: { available: false, installed: false } }) } as unknown as ApiClient;
    renderWithProviders(<DemoCard client={client} />);
    await waitFor(() => expect(client.getDemo).toHaveBeenCalled());
    expect(screen.queryByRole("button", { name: /load demo/i })).toBeNull();
  });

  it("installs the demo and links to the agent", async () => {
    const getDemo = vi
      .fn()
      .mockResolvedValueOnce({ dental: { available: true, installed: false } })
      .mockResolvedValue({ dental: { available: true, installed: true, agent_id: "f1" } });
    const installDentalDemo = vi.fn().mockResolvedValue({ mcp_server_id: "m", mcp_created: true, tools: 7, agent_id: "f1", agent_created: true });
    renderWithProviders(<DemoCard client={{ getDemo, installDentalDemo } as unknown as ApiClient} />);
    await userEvent.click(await screen.findByRole("button", { name: /load demo/i }));
    expect(installDentalDemo).toHaveBeenCalledTimes(1);
    expect(await screen.findByRole("link", { name: /smile dental front desk/i })).toHaveAttribute("href", "/agents/f1");
  });

  it("shows the failure and lets the user retry", async () => {
    const installDentalDemo = vi.fn().mockRejectedValue(new Error("boom"));
    const client = { getDemo: vi.fn().mockResolvedValue({ dental: { available: true, installed: false } }), installDentalDemo } as unknown as ApiClient;
    renderWithProviders(<DemoCard client={client} />);
    await userEvent.click(await screen.findByRole("button", { name: /load demo/i }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/could not be installed/i);
    expect(screen.getByRole("button", { name: /load demo/i })).toBeEnabled();
  });
});
