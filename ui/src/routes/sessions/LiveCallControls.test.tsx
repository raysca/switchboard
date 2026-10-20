import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ApiError, type ApiClient } from "../../api/client";
import { LiveCallControls } from "./LiveCallControls";

const NOTE = "the caller is a VIP";

function setup(client: Partial<ApiClient> = {}) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const full = {
    addSessionNote: vi.fn().mockResolvedValue({ accepted: 1 }),
    runSession: vi.fn().mockResolvedValue({ accepted: 1 }),
    ...client,
  } as unknown as ApiClient;
  const onEnded = vi.fn();
  render(
    <QueryClientProvider client={qc}>
      <LiveCallControls client={full} sessionId="s1" onEnded={onEnded} />
    </QueryClientProvider>,
  );
  return { client: full, onEnded, qc, user: userEvent.setup() };
}
const fail = (status: number, code: string, message = "server says") => new ApiError(message, code, status, "r");

describe("LiveCallControls", () => {
  it("has labelled controls and keeps Send disabled for a blank note", async () => {
    const { user } = setup();
    expect(screen.getByRole("region", { name: "Guide this call" })).toBeVisible();
    const send = screen.getByRole("button", { name: "Send note" });
    expect(send).toBeDisabled();
    await user.type(screen.getByLabelText("Note for the agent"), "   ");
    expect(send).toBeDisabled();
    expect(screen.getByRole("checkbox", { name: "Reply now" })).not.toBeChecked();
    expect(screen.getByRole("button", { name: "Ask for a reply" })).toBeEnabled();
    expect(screen.getByText(/The note itself is not saved; the agent's reply is\./)).toBeVisible();
  });

  it("sends the note, clears the box, keeps focus and says so", async () => {
    const { client, user } = setup();
    const box = screen.getByLabelText("Note for the agent");
    await user.type(box, NOTE);
    await user.click(screen.getByRole("button", { name: "Send note" }));
    expect(client.addSessionNote).toHaveBeenCalledWith("s1", NOTE, false);
    expect(await screen.findByText("Note sent.")).toBeVisible();
    expect(box).toHaveValue("");
    expect(box).toHaveFocus();
  });

  it("sends run: true when Reply now is checked", async () => {
    const { client, user } = setup();
    await user.type(screen.getByLabelText("Note for the agent"), NOTE);
    await user.click(screen.getByRole("checkbox", { name: "Reply now" }));
    await user.click(screen.getByRole("button", { name: "Send note" }));
    expect(client.addSessionNote).toHaveBeenCalledWith("s1", NOTE, true);
  });

  it("unchecks Reply now after a successful send, so the next note does not force a reply", async () => {
    const { client, user } = setup();
    await user.type(screen.getByLabelText("Note for the agent"), NOTE);
    await user.click(screen.getByRole("checkbox", { name: "Reply now" }));
    await user.click(screen.getByRole("button", { name: "Send note" }));
    await screen.findByText("Note sent.");
    expect(screen.getByRole("checkbox", { name: "Reply now" })).not.toBeChecked();
    await user.type(screen.getByLabelText("Note for the agent"), "again");
    await user.click(screen.getByRole("button", { name: "Send note" }));
    await waitFor(() => expect(client.addSessionNote).toHaveBeenLastCalledWith("s1", "again", false));
  });

  it("counts the server's bytes, not characters: 1500 CJK characters are too long", async () => {
    const { client, user } = setup();
    const box = screen.getByLabelText("Note for the agent");
    await user.click(box);
    await user.paste("汉".repeat(1500)); // 4500 bytes, 1500 UTF-16 units
    expect(screen.getByRole("button", { name: "Send note" })).toBeDisabled();
    expect(screen.getByText(/4500 bytes.*4096/)).toBeVisible();
    await user.clear(box);
    await user.paste("汉".repeat(1365)); // 4095 bytes
    expect(screen.getByRole("button", { name: "Send note" })).toBeEnabled();
    expect(screen.queryByText(/bytes/)).toBeNull();
    expect(client.addSessionNote).not.toHaveBeenCalled();
  });

  it("asks for a reply", async () => {
    const { client, user } = setup();
    await user.click(screen.getByRole("button", { name: "Ask for a reply" }));
    expect(client.runSession).toHaveBeenCalledWith("s1");
    expect(await screen.findByText("Reply requested.")).toBeVisible();
  });

  it("on 404 says the call ended and tells the page once", async () => {
    const { onEnded, user } = setup({ addSessionNote: vi.fn().mockRejectedValue(fail(404, "not_found")) });
    await user.type(screen.getByLabelText("Note for the agent"), NOTE);
    await user.click(screen.getByRole("button", { name: "Send note" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("The call has ended, or it runs on another server instance.");
    expect(onEnded).toHaveBeenCalledTimes(1);
  });

  it.each([
    [409, "call_not_ready", "The call is still starting. Try again in a moment."],
    [429, "rate_limited", "Too many requests for this call. Wait a second and try again."],
    [400, "invalid_request", "note is too long"],
  ])("shows the copy for %i", async (status, code, copy) => {
    const { onEnded, user } = setup({ runSession: vi.fn().mockRejectedValue(fail(status, code, "note is too long")) });
    await user.click(screen.getByRole("button", { name: "Ask for a reply" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(copy);
    expect(onEnded).not.toHaveBeenCalled();
  });

  it("never echoes the note in an error, and keeps it for a retry", async () => {
    const { user } = setup({ addSessionNote: vi.fn().mockRejectedValue(fail(409, "call_not_ready")) });
    const box = screen.getByLabelText("Note for the agent");
    await user.type(box, NOTE);
    await user.click(screen.getByRole("button", { name: "Send note" }));
    expect(await screen.findByRole("alert")).not.toHaveTextContent(NOTE);
    expect(box).toHaveValue(NOTE);
  });

  it("keeps the note out of the URL, localStorage, console and the mutation cache", async () => {
    const setItem = vi.spyOn(Storage.prototype, "setItem");
    const spies = (["log", "info", "warn", "error", "debug"] as const).map((m) => vi.spyOn(console, m));
    const { qc, user } = setup();
    await user.type(screen.getByLabelText("Note for the agent"), NOTE);
    await user.click(screen.getByRole("button", { name: "Send note" }));
    await screen.findByText("Note sent.");
    expect(document.location.href).not.toContain("VIP");
    expect(setItem).not.toHaveBeenCalled();
    for (const s of spies) expect(JSON.stringify(s.mock.calls)).not.toContain(NOTE);
    expect(JSON.stringify(qc.getMutationCache().getAll().map((m) => m.state.variables))).not.toContain(NOTE);
    vi.restoreAllMocks();
  });

  it("tabs textarea, checkbox, Send, Ask", async () => {
    const { user } = setup();
    await user.type(screen.getByLabelText("Note for the agent"), NOTE);
    await user.tab();
    expect(screen.getByRole("checkbox", { name: "Reply now" })).toHaveFocus();
    await user.tab();
    expect(screen.getByRole("button", { name: "Send note" })).toHaveFocus();
    await user.tab();
    expect(screen.getByRole("button", { name: "Ask for a reply" })).toHaveFocus();
  });
});
