import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { SecretsView } from "./SecretsView";
import { ApiError, type ApiClient, type SecretChangeDTO, type SecretDTO } from "../../api/client";

const env: SecretDTO = { name: "MCP_TOKEN_CRM", source: "environment", writable: false };
const stored: SecretDTO = { name: "crm-api", source: "store", writable: true, updated_at: "2026-10-24T09:00:00Z", last_used_at: null };

function setup(client: Partial<ApiClient> = {}, list: { store_enabled: boolean; secrets: SecretDTO[]; recent_changes?: SecretChangeDTO[] } = { store_enabled: true, secrets: [env, stored] }) {
  const full = { getSecrets: vi.fn().mockResolvedValue({ recent_changes: [], ...list }), putSecret: vi.fn().mockResolvedValue(stored), deleteSecret: vi.fn().mockResolvedValue(undefined), ...client } as unknown as ApiClient;
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>
        <SecretsView client={full} keyValue="k" />
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return full;
}

describe("SecretsView", () => {
  it("lists names and where they live, with actions only for stored secrets", async () => {
    setup();
    const table = await screen.findByRole("table", { name: "Secrets available to tools and MCP servers" });
    expect(within(table).getByText("MCP_TOKEN_CRM")).toBeVisible();
    expect(within(table).getByText("Environment variable")).toBeVisible();
    expect(within(table).getByText("Encrypted store")).toBeVisible();
    expect(within(table).getAllByRole("button", { name: "Delete" })).toHaveLength(1);
  });

  it("adds a secret: validates the name, sends the value once, and never shows it", async () => {
    const client = setup();
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "Add secret" }));
    await user.type(screen.getByLabelText("Name"), "Bad Name");
    await user.type(screen.getByLabelText("Value"), "s3cret-value");
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Name: lower-case letters");
    expect(client.putSecret).not.toHaveBeenCalled();
    await user.clear(screen.getByLabelText("Name"));
    await user.type(screen.getByLabelText("Name"), "billing-api");
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(client.putSecret).toHaveBeenCalledWith("billing-api", "s3cret-value"));
    expect(screen.queryByText("s3cret-value")).toBeNull();
  });

  it("explains an off store and hides Add secret", async () => {
    setup({}, { store_enabled: false, secrets: [env] });
    expect(await screen.findByText(/The encrypted store is off/)).toBeVisible();
    expect(screen.queryByRole("button", { name: "Add secret" })).toBeNull();
  });

  it("says what still uses a secret when delete is refused", async () => {
    const deleteSecret = vi.fn().mockRejectedValue(new ApiError("secret is used by tools or servers", "secret_in_use", 409, "req-1", [{ kind: "mcp_server", id: "s1", name: "CRM" }]));
    setup({ deleteSecret });
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "Delete" }));
    await user.click(screen.getByRole("button", { name: "Delete secret" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Still used by: CRM");
  });

  it("lists recent changes by name and request, never a value", async () => {
    const recent_changes: SecretChangeDTO[] = [
      { at: new Date(Date.now() - 2 * 60_000).toISOString(), action: "replace", name: "crm-api", request_id: "0123456789abcdef" },
      { at: new Date(Date.now() - 3600_000).toISOString(), action: "create", name: "crm-api", request_id: "fedcba9876543210" },
    ];
    setup({}, { store_enabled: true, secrets: [env, stored], recent_changes });
    const section = await screen.findByRole("region", { name: "Recent changes" });
    expect(within(section).getAllByRole("listitem")).toHaveLength(2);
    expect(section).toHaveTextContent("crm-api was replaced");
    expect(section).toHaveTextContent("request 01234567");
    expect(section).toHaveTextContent("crm-api was added");
  });

  it("shows no changes section until something has changed", async () => {
    setup();
    await screen.findByRole("table");
    expect(screen.queryByRole("region", { name: "Recent changes" })).toBeNull();
  });
});
