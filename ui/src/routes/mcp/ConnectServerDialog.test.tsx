import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ConnectServerDialog } from "./ConnectServerDialog";

function setup(over: Partial<{ pending: boolean; error: string | null }> = {}) {
  const onSubmit = vi.fn();
  const onCancel = vi.fn();
  render(<ConnectServerDialog pending={false} error={null} onSubmit={onSubmit} onCancel={onCancel} {...over} />);
  return { onSubmit, onCancel, user: userEvent.setup() };
}

describe("ConnectServerDialog", () => {
  it("starts in the name field, so typing goes straight in", () => {
    setup();
    expect(screen.getByLabelText("Name")).toHaveFocus();
  });

  it("asks for a name and an endpoint, without sending anything", async () => {
    const { onSubmit, user } = setup();
    await user.click(screen.getByRole("button", { name: "Connect" }));
    expect(screen.getByRole("alert")).toHaveTextContent("Server name is required");
    await user.type(screen.getByLabelText("Name"), "crm");
    await user.click(screen.getByRole("button", { name: "Connect" }));
    expect(screen.getByRole("alert")).toHaveTextContent("Endpoint URL is required");
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it("blocks a token variable that is not an MCP_TOKEN_ name", async () => {
    const { onSubmit, user } = setup();
    await user.type(screen.getByLabelText("Name"), "crm");
    await user.type(screen.getByLabelText("Endpoint URL"), "https://mcp.example.com");
    await user.type(screen.getByLabelText("Token variable (optional)"), "OPENAI_API_KEY");
    await user.click(screen.getByRole("button", { name: "Connect" }));
    expect(screen.getByRole("alert")).toHaveTextContent("Token variable must match MCP_TOKEN_ followed by A-Z, 0-9 or _ (max 48).");
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it("sends the trimmed fields, streamable HTTP, and leaves out what was left empty", async () => {
    const { onSubmit, user } = setup();
    await user.type(screen.getByLabelText("Name"), "  crm  ");
    await user.type(screen.getByLabelText("Endpoint URL"), "https://mcp.example.com/rpc");
    await user.click(screen.getByRole("button", { name: "Connect" }));
    expect(onSubmit).toHaveBeenCalledWith({ name: "crm", description: undefined, endpoint: "https://mcp.example.com/rpc", transport: "streamable-http", auth_env: undefined, enabled: true });
  });

  it("sends a valid token variable, a description and enabled off", async () => {
    const { onSubmit, user } = setup();
    await user.type(screen.getByLabelText("Name"), "billing");
    await user.type(screen.getByLabelText("Endpoint URL"), "https://mcp.example.com/billing");
    await user.type(screen.getByLabelText("Description (optional)"), "Invoices");
    await user.type(screen.getByLabelText("Token variable (optional)"), "MCP_TOKEN_BILLING");
    await user.click(screen.getByLabelText("Enabled for flows"));
    await user.click(screen.getByRole("button", { name: "Connect" }));
    expect(onSubmit).toHaveBeenCalledWith({ name: "billing", description: "Invoices", endpoint: "https://mcp.example.com/billing", transport: "streamable-http", auth_env: "MCP_TOKEN_BILLING", enabled: false });
  });

  it("shows the server's error, disables Connect while sending, and cancels", async () => {
    const { onCancel, user } = setup({ pending: true, error: "MCP server configuration is invalid (Request ID: r1)" });
    expect(screen.getByRole("alert")).toHaveTextContent("MCP server configuration is invalid (Request ID: r1)");
    expect(screen.getByRole("button", { name: "Connecting…" })).toBeDisabled();
    await user.click(screen.getByRole("button", { name: "Cancel" }));
    expect(onCancel).toHaveBeenCalledOnce();
  });
});
