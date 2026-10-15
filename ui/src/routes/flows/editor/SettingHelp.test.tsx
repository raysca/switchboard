import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { SettingHelp } from "./SettingHelp";

describe("SettingHelp", () => {
  it("starts closed, opens a labelled region with the parts the entry has, and toggles", async () => {
    const user = userEvent.setup();
    render(<SettingHelp id="vad_stop_ms" />);
    const button = screen.getByRole("button", { name: "Explain Wait after the caller stops" });
    expect(button).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByRole("region")).toBeNull();

    await user.click(button);
    expect(button).toHaveAttribute("aria-expanded", "true");
    const region = screen.getByRole("region", { name: "Explain Wait after the caller stops" });
    expect(region).toHaveTextContent("What it does");
    expect(region).toHaveTextContent("When to change it");
    expect(region).toHaveTextContent("Default and range");
    expect(region).toHaveTextContent("Careful");

    await user.click(button);
    expect(screen.queryByRole("region")).toBeNull();
  });

  it("leaves out the parts an entry does not have", async () => {
    const user = userEvent.setup();
    render(<SettingHelp id="tts.clean_text" />);
    await user.click(screen.getByRole("button", { name: /^Explain/ }));
    expect(screen.getByRole("region")).not.toHaveTextContent("Careful");
  });

  it("opens from the keyboard and Escape closes it and returns focus to the button", async () => {
    const user = userEvent.setup();
    render(<SettingHelp id="llm.max_parallel" />);
    const button = screen.getByRole("button", { name: /^Explain/ });
    button.focus();
    await user.keyboard("{Enter}");
    expect(screen.getByRole("region")).toBeInTheDocument();
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("region")).toBeNull();
    expect(button).toHaveFocus();
  });

  it("opens each instance independently", async () => {
    const user = userEvent.setup();
    render(
      <>
        <SettingHelp id="vad_stop_ms" />
        <SettingHelp id="llm.max_parallel" />
      </>,
    );
    await user.click(screen.getByRole("button", { name: "Explain Tool calls at once" }));
    expect(screen.getAllByRole("region")).toHaveLength(1);
    await user.click(screen.getByRole("button", { name: "Explain Wait after the caller stops" }));
    expect(screen.getAllByRole("region")).toHaveLength(2);
  });
});
