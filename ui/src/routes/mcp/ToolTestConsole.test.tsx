import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ApiError, type ApiClient } from "../../api/client";
import { ToolTestConsole } from "./ToolTestConsole";

const result = { execution_id: "exec-9999-abcdef", mcp_server_id: "crm", tool_name: "lookup", status: "completed", duration_ms: 42, result: { name: "Alice" } };
function setup(testMCPTool = vi.fn().mockResolvedValue(result)) {
  const client = { testMCPTool } as unknown as ApiClient;
  render(<ToolTestConsole client={client} serverId="crm" tool="lookup" />);
  return { testMCPTool, user: userEvent.setup() };
}
const args = () => screen.getByLabelText("JSON arguments (max 64 KiB)") as HTMLTextAreaElement;

describe("ToolTestConsole", () => {
  it("runs the tool once with the arguments and shows status, time, execution and the result", async () => {
    const { testMCPTool, user } = setup();
    fireEvent.change(args(), { target: { value: '{ "id": "c1" }' } });
    await user.click(screen.getByRole("button", { name: "Run tool" }));
    expect(testMCPTool).toHaveBeenCalledWith("crm", "lookup", { id: "c1" });
    expect(await screen.findByText("completed")).toBeVisible();
    expect(screen.getByText("42 ms")).toBeVisible();
    expect(screen.getByText("exec-999…")).toBeVisible();
    expect(screen.getByText(/"name": "Alice"/)).toBeVisible();
  });

  it("starts with an empty object", async () => {
    const { testMCPTool, user } = setup();
    expect(args().value).toBe("{}");
    await user.click(screen.getByRole("button", { name: "Run tool" }));
    expect(testMCPTool).toHaveBeenCalledWith("crm", "lookup", {});
  });

  it("refuses arguments that are not a JSON object, without calling the server", async () => {
    const { testMCPTool, user } = setup();
    for (const [text, message] of [
      ["{nope", "Arguments must be valid JSON"],
      ["[1, 2]", "Arguments must be a valid JSON object"],
      ["42", "Arguments must be a valid JSON object"],
      ["null", "Arguments must be a valid JSON object"],
    ] as const) {
      fireEvent.change(args(), { target: { value: text } });
      await user.click(screen.getByRole("button", { name: "Run tool" }));
      expect(screen.getByRole("alert")).toHaveTextContent(message);
    }
    expect(testMCPTool).not.toHaveBeenCalled();
  });

  it("refuses arguments over 64 KiB", async () => {
    const { testMCPTool, user } = setup();
    fireEvent.change(args(), { target: { value: JSON.stringify({ blob: "x".repeat(65_536) }) } });
    await user.click(screen.getByRole("button", { name: "Run tool" }));
    expect(screen.getByRole("alert")).toHaveTextContent("Arguments exceed the 64 KiB execution limit");
    expect(testMCPTool).not.toHaveBeenCalled();
  });

  it("shows a failure with its request id, and clears the last result when run again", async () => {
    const testMCPTool = vi.fn().mockResolvedValueOnce(result).mockRejectedValueOnce(new ApiError("tool blew up", "mcp_tool_failed", 502, "req-77"));
    const { user } = setup(testMCPTool);
    await user.click(screen.getByRole("button", { name: "Run tool" }));
    expect(await screen.findByText("completed")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Run tool" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("tool blew up (Request ID: req-77)");
    expect(screen.queryByText("completed")).toBeNull();
  });

  it("runs one call at a time", async () => {
    let finish: (v: typeof result) => void = () => {};
    const { testMCPTool, user } = setup(vi.fn().mockReturnValue(new Promise((r) => (finish = r))));
    await user.click(screen.getByRole("button", { name: "Run tool" }));
    expect(screen.getByRole("button", { name: "Running…" })).toBeDisabled();
    finish(result);
    expect(await screen.findByText("completed")).toBeVisible();
    expect(testMCPTool).toHaveBeenCalledTimes(1);
  });

  it("marks a result that did not complete as a failure", async () => {
    setup(vi.fn().mockResolvedValue({ ...result, status: "failed" }));
    await userEvent.click(screen.getByRole("button", { name: "Run tool" }));
    expect(await screen.findByText("failed")).toBeVisible();
  });
});
