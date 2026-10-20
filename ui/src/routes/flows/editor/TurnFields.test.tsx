import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import type { ServerConfigDTO } from "../../../api/client";
import { TurnFields } from "./TurnFields";
import type { StepConfig } from "./model";

const server = (turn: Partial<ServerConfigDTO["turn"]> = {}): ServerConfigDTO => ({
  profile: "development",
  max_sessions: 10,
  max_call_duration_seconds: 1800,
  voice_token_max_lifetime_seconds: 60,
  client_ip: "",
  secure: false,
  audio: { backgrounds: [], cues: [], allow_flow_background: true, background_default: "" },
  idle: { user_timeout_ms: 0, max_prompts: 2, prompt_mode: "llm" },
  turn: { manager: true, min_words: 0, mute: [], max_mute_ms: 30000, speech_timeout_ms: 600, ...turn },
});

/** Holds the config like FlowSettingsPanel: a null value removes the key. */
function Harness({ initial = {}, serverConfig = server(), onSet = vi.fn() }: { initial?: StepConfig; serverConfig?: ServerConfigDTO; onSet?: (cfg: StepConfig) => void }) {
  const [cfg, setCfg] = useState<StepConfig>(initial);
  return (
    <TurnFields
      cfg={cfg}
      serverConfig={serverConfig}
      setKey={(key, value) => {
        const { [key]: _drop, ...rest } = cfg;
        const next = value === null || value === undefined ? rest : { ...rest, [key]: value };
        setCfg(next);
        onSet(next);
      }}
    />
  );
}
const last = (fn: ReturnType<typeof vi.fn>) => fn.mock.calls.at(-1)![0] as StepConfig;
const radio = (name: RegExp | string) => screen.getByRole("radio", { name });

describe("TurnFields", () => {
  it("is a labelled group of four choices, the server's setting selected and described", () => {
    render(<Harness />);
    expect(screen.getByRole("group", { name: "Interruptions" })).toBeVisible();
    expect(screen.getByRole("group", { name: "When can the caller interrupt?" })).toBeVisible();
    expect(radio("Server setting (any speech interrupts)")).toBeChecked();
    for (const n of ["Any time", "After a number of words", "Never while the agent speaks"]) expect(radio(n)).not.toBeChecked();
    expect(screen.getByText(/Muted speech still reaches speech-to-text/)).toBeVisible();
  });

  it("describes a server that waits for words or mutes", () => {
    const { unmount } = render(<Harness serverConfig={server({ min_words: 3 })} />);
    expect(radio("Server setting (after 3 words)")).toBeChecked();
    unmount();
    render(<Harness serverConfig={server({ mute: ["always"] })} />);
    expect(radio("Server setting (never while the agent speaks)")).toBeChecked();
  });

  it("any time writes min_words 0, and the server setting removes turn again", async () => {
    const user = userEvent.setup();
    const onSet = vi.fn();
    render(<Harness onSet={onSet} />);
    await user.click(radio("Any time"));
    expect(last(onSet).turn).toEqual({ min_words: 0 });
    await user.click(radio(/^Server setting/));
    expect(last(onSet)).not.toHaveProperty("turn");
  });

  it("after N words writes the number and keeps it in range", async () => {
    const user = userEvent.setup();
    const onSet = vi.fn();
    render(<Harness onSet={onSet} />);
    const words = screen.getByRole("spinbutton", { name: "Words before the caller interrupts" });
    expect(words).toBeDisabled();
    await user.click(radio("After a number of words"));
    expect(last(onSet).turn).toEqual({ min_words: 2 });
    expect(words).toBeEnabled();
    expect(words).toHaveAttribute("min", "1");
    expect(words).toHaveAttribute("max", "10");
    await user.clear(words);
    await user.type(words, "3");
    expect(last(onSet).turn).toEqual({ min_words: 3 });
    expect(screen.getByText(/Needs a speech-to-text with interim results\./)).toBeVisible();
  });

  it("never while the agent speaks mutes always, and drops the word count", async () => {
    const user = userEvent.setup();
    const onSet = vi.fn();
    render(<Harness initial={{ turn: { min_words: 3 } }} onSet={onSet} />);
    await user.click(radio("Never while the agent speaks"));
    expect(last(onSet).turn).toEqual({ mute: ["always"] });
    await user.click(radio(/^Server setting/));
    expect(last(onSet)).not.toHaveProperty("turn");
  });

  it("any time overrides a server that mutes: an explicit empty list", async () => {
    const user = userEvent.setup();
    const onSet = vi.fn();
    render(<Harness serverConfig={server({ mute: ["always"] })} onSet={onSet} />);
    await user.click(radio("Any time"));
    expect(last(onSet).turn).toEqual({ min_words: 0, mute: [] });
    await user.click(radio(/^Server setting/));
    expect(last(onSet)).not.toHaveProperty("turn");
  });

  it("protect the first reply and not while a tool runs write the list, and unticking both removes turn", async () => {
    const user = userEvent.setup();
    const onSet = vi.fn();
    render(<Harness onSet={onSet} />);
    await user.click(screen.getByRole("checkbox", { name: "Protect the agent's first reply or greeting" }));
    expect(last(onSet).turn).toEqual({ mute: ["first_speech"] });
    await user.click(screen.getByRole("checkbox", { name: "Not while a tool runs" }));
    expect(last(onSet).turn).toEqual({ mute: ["first_speech", "function_call"] });
    await user.click(screen.getByRole("checkbox", { name: "Protect the agent's first reply or greeting" }));
    await user.click(screen.getByRole("checkbox", { name: "Not while a tool runs" }));
    expect(last(onSet)).not.toHaveProperty("turn");
  });

  it("starts the checkboxes from the server's list and keeps its other strategies when one changes", async () => {
    const user = userEvent.setup();
    const onSet = vi.fn();
    render(<Harness serverConfig={server({ mute: ["function_call"] })} onSet={onSet} />);
    expect(screen.getByRole("checkbox", { name: "Not while a tool runs" })).toBeChecked();
    await user.click(screen.getByRole("checkbox", { name: "Protect the agent's first reply or greeting" }));
    expect(last(onSet).turn).toEqual({ mute: ["first_speech", "function_call"] });
  });

  it("shows a stored until_first_bot_complete as a note and keeps it", async () => {
    const user = userEvent.setup();
    const onSet = vi.fn();
    render(<Harness initial={{ turn: { mute: ["until_first_bot_complete"] } }} onSet={onSet} />);
    expect(screen.getByText(/also muted until the greeting has finished/i)).toBeVisible();
    await user.click(screen.getByRole("checkbox", { name: "Not while a tool runs" }));
    expect(last(onSet).turn).toEqual({ mute: ["until_first_bot_complete", "function_call"] });
  });

  it("longest mute is in seconds, bounded by the server, empty means the server's", async () => {
    const user = userEvent.setup();
    const onSet = vi.fn();
    render(<Harness serverConfig={server({ max_mute_ms: 20000 })} onSet={onSet} />);
    const secs = screen.getByRole("spinbutton", { name: "Longest mute (seconds)" });
    expect(secs).toHaveAttribute("placeholder", "20");
    expect(secs).toHaveAttribute("min", "1");
    expect(secs).toHaveAttribute("max", "20");
    await user.type(secs, "15");
    expect(last(onSet).turn).toEqual({ max_mute_ms: 15000 });
    await user.clear(secs);
    expect(last(onSet)).not.toHaveProperty("turn");
  });

  it("caps the longest mute field at 60 seconds on a server with a longer limit", () => {
    render(<Harness serverConfig={server({ max_mute_ms: 120000 })} />);
    expect(screen.getByRole("spinbutton", { name: "Longest mute (seconds)" })).toHaveAttribute("max", "60");
  });

  it("is disabled, and says why, while the server has the turn manager off", () => {
    render(<Harness serverConfig={server({ manager: false })} />);
    expect(screen.getByText("This server has the turn manager off.")).toBeVisible();
    for (const el of [...screen.getAllByRole("radio"), ...screen.getAllByRole("checkbox"), ...screen.getAllByRole("spinbutton")]) expect(el).toBeDisabled();
  });

  it("reads keyboard-first: the group, the checkboxes, then the longest mute", async () => {
    const user = userEvent.setup();
    render(<Harness />);
    await user.tab();
    expect(radio(/^Server setting/)).toHaveFocus();
    await user.tab();
    expect(screen.getByRole("button", { name: "Explain When can the caller interrupt?" })).toHaveFocus();
  });

  it("works without the server's config (still loading)", async () => {
    const user = userEvent.setup();
    const onSet = vi.fn();
    render(<TurnFields cfg={{}} setKey={(k, v) => onSet(k, v)} />);
    await user.click(radio("Any time"));
    expect(onSet).toHaveBeenLastCalledWith("turn", { min_words: 0 });
  });
});
