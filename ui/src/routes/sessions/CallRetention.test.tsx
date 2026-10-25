import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router";
import { ApiError, type ApiClient, type SessionDTO } from "../../api/client";
import { CallRetention } from "./CallRetention";

const ID = "0192aaaa-0000-7000-8000-000000000001";
const session = (over: Partial<SessionDTO> = {}): SessionDTO => ({
  id: ID,
  agent_id: "a1",
  agent_version_id: "v1",
  config_hash: "h",
  started_at: "2026-10-12T14:00:00Z",
  ended_at: "2026-10-12T14:04:12Z",
  status: "ended",
  turn_count: 1,
  safe_summary: "",
  draft: false,
  caller: "",
  end_reason: "",
  held: false,
  hold_reason: "",
  ...over,
});
const erased = { turns: 1, tool_calls: 0, spans: 0, events: 0, recordings_removed: 1 };

function setup(s: SessionDTO, client: Partial<ApiClient> = {}) {
  const full = { holdSession: vi.fn().mockResolvedValue(session({ held: true, hold_reason: "legal" })), releaseSession: vi.fn().mockResolvedValue(session()), eraseSession: vi.fn().mockResolvedValue(erased), ...client } as unknown as ApiClient;
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <MemoryRouter initialEntries={[`/sessions/${ID}`]}>
        <Routes>
          <Route path="/sessions" element={<p>Calls list</p>} />
          <Route path="/sessions/:id" element={<CallRetention client={full} session={s} />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return { client: full, user: userEvent.setup() };
}

describe("CallRetention", () => {
  it("holds a call with the reason typed", async () => {
    const { client, user } = setup(session());
    await user.click(screen.getByRole("button", { name: "Hold this call…" }));
    const dialog = screen.getByRole("dialog", { name: "Hold this call?" });
    await user.type(within(dialog).getByLabelText("Reason (optional)"), "  legal case 42 ");
    await user.click(within(dialog).getByRole("button", { name: "Hold call" }));
    expect(client.holdSession).toHaveBeenCalledWith(ID, "legal case 42");
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("refuses a reason over the server's byte limit", async () => {
    const { client, user } = setup(session());
    await user.click(screen.getByRole("button", { name: "Hold this call…" }));
    await user.type(screen.getByLabelText("Reason (optional)"), "é".repeat(101)); // 202 bytes
    expect(screen.getByRole("alert")).toHaveTextContent("202 bytes; the limit is 200");
    expect(screen.getByRole("button", { name: "Hold call" })).toBeDisabled();
    expect(client.holdSession).not.toHaveBeenCalled();
  });

  it("shows a hold with its reason, releases it, and will not erase a held call", async () => {
    const { client, user } = setup(session({ held: true, hold_reason: "legal case 42" }));
    expect(screen.getByText("On hold")).toBeVisible();
    expect(screen.getByText(/legal case 42/)).toBeVisible();
    expect(screen.getByRole("button", { name: "Erase this call…" })).toBeDisabled();
    expect(screen.getByText("Release the hold to erase this call.")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Release hold" }));
    expect(client.releaseSession).toHaveBeenCalledWith(ID);
  });

  it("will not erase a call that is still running", () => {
    setup(session({ status: "active", ended_at: null }));
    expect(screen.getByRole("button", { name: "Erase this call…" })).toBeDisabled();
    expect(screen.getByText("A call can be erased once it has ended.")).toBeVisible();
  });

  it("names a failed hold in the dialog", async () => {
    const { user } = setup(session(), { holdSession: vi.fn().mockRejectedValue(new ApiError("session not found", "not_found", 404, "r")) });
    await user.click(screen.getByRole("button", { name: "Hold this call…" }));
    await user.click(screen.getByRole("button", { name: "Hold call" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("session not found");
  });

  it("erases only after the call's name is typed, then returns to the list", async () => {
    const { client, user } = setup(session());
    await user.click(screen.getByRole("button", { name: "Erase this call…" }));
    const dialog = screen.getByRole("dialog", { name: "Erase call_0192aaaa?" });
    expect(within(dialog).getByText(/cannot be undone/)).toBeVisible();
    const go = within(dialog).getByRole("button", { name: "Erase call" });
    expect(go).toBeDisabled();
    await user.type(within(dialog).getByLabelText(/Type/), "call_0192aaa");
    expect(go).toBeDisabled();
    await user.type(within(dialog).getByLabelText(/Type/), "a");
    await user.click(go);
    expect(client.eraseSession).toHaveBeenCalledWith(ID);
    expect(await screen.findByText("Calls list")).toBeVisible();
  });

  it("keeps the dialog and says so when a recording could not be removed, and finishes on a retry", async () => {
    const eraseSession = vi
      .fn()
      .mockRejectedValueOnce(new ApiError("the call's rows are erased but a recording file could not be removed: erase the same id again to retry", "recordings_not_removed", 500, "r"))
      .mockRejectedValueOnce(new ApiError("session not found", "not_found", 404, "r"));
    const { user } = setup(session(), { eraseSession });
    await user.click(screen.getByRole("button", { name: "Erase this call…" }));
    await user.type(screen.getByLabelText(/Type/), "call_0192aaaa");
    await user.click(screen.getByRole("button", { name: "Erase call" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("erase the same id again to retry");
    expect(screen.getByRole("dialog")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Erase call" }));
    expect(await screen.findByText("Calls list")).toBeVisible();
  });

  it("says why a held call was refused", async () => {
    const { user } = setup(session(), { eraseSession: vi.fn().mockRejectedValue(new ApiError("the call is on hold: release it before erasing it", "session_held", 409, "r")) });
    await user.click(screen.getByRole("button", { name: "Erase this call…" }));
    await user.type(screen.getByLabelText(/Type/), "call_0192aaaa");
    await user.click(screen.getByRole("button", { name: "Erase call" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("on hold");
  });
});
