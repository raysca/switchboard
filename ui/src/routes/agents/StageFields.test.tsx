import { useState } from "react";
import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ProviderStatus } from "../../api/client";
import { StageFields, type StageKind, type StageValue } from "./StageFields";

const svc = (extra: object) => ({ available: true, custom: true, ...extra });
const google: ProviderStatus = {
  id: "google",
  display_name: "Google",
  capabilities: ["stt", "llm", "tts"],
  detected: true,
  env: ["GOOGLE_API_KEY"],
  services: {
    stt: svc({
      models: [{ id: "chirp_2", label: "Chirp 2", default: true }],
      languages: [
        { id: "en-US", label: "English (US)", default: true },
        { id: "fr-FR", label: "French" },
      ],
    }),
    llm: svc({ models: [{ id: "gemini-flash", label: "Gemini Flash", default: true }, { id: "gemini-pro", label: "Gemini Pro" }] }),
    tts: svc({ voices: [{ id: "aoede", label: "Aoede", default: true }] }),
  },
};
const xai: ProviderStatus = {
  id: "xai",
  display_name: "xAI",
  capabilities: ["llm", "tts"],
  detected: false,
  env: ["XAI_API_KEY"],
  services: {
    llm: { available: false, reason: "missing_env", missing_env: ["XAI_API_KEY"], custom: true },
    tts: { available: false, reason: "missing_env", missing_env: ["XAI_API_KEY"], custom: true },
  },
};

function Harness({ kind, providers, initial, spy }: { kind: StageKind; providers: ProviderStatus[]; initial: StageValue; spy?: (v: StageValue) => void }) {
  const [v, setV] = useState(initial);
  return (
    <StageFields
      kind={kind}
      providers={providers}
      value={v}
      idPrefix="t"
      onChange={(n) => {
        spy?.(n);
        setV(n);
      }}
    />
  );
}

describe("StageFields", () => {
  it("lists only available providers", () => {
    render(<Harness kind="llm" providers={[google, xai]} initial={{ provider: "" }} />);
    const opts = within(screen.getByLabelText("LLM provider")).getAllByRole("option").map((o) => o.textContent);
    expect(opts).toEqual(["Select a provider", "Google"]);
  });

  it("fills catalog defaults when a provider is chosen", async () => {
    const spy = vi.fn();
    render(<Harness kind="stt" providers={[google]} initial={{ provider: "" }} spy={spy} />);
    await userEvent.selectOptions(screen.getByLabelText("STT provider"), "google");
    expect(spy).toHaveBeenLastCalledWith({ provider: "google", model: "chirp_2", language: "en-US" });
    expect(screen.getByLabelText("STT language")).toHaveValue("en-US");
  });

  it("offers Other… last and sends the typed value", async () => {
    const spy = vi.fn();
    render(<Harness kind="llm" providers={[google]} initial={{ provider: "google", model: "gemini-flash" }} spy={spy} />);
    const select = screen.getByLabelText("LLM model");
    expect(within(select).getAllByRole("option").at(-1)).toHaveTextContent("Other…");
    await userEvent.selectOptions(select, "Other…");
    await userEvent.type(screen.getByLabelText("LLM model (custom)"), "x");
    expect(spy).toHaveBeenLastCalledWith({ provider: "google", model: "x" });
  });

  it("shows a value missing from the catalog as Other…", () => {
    render(<Harness kind="tts" providers={[google]} initial={{ provider: "google", voice: "my-voice" }} />);
    expect(screen.getByLabelText("Voice")).toHaveDisplayValue("Other…");
    expect(screen.getByLabelText("Voice (custom)")).toHaveValue("my-voice");
  });

  it("hides Other… when the provider disallows custom values", () => {
    const strict = { ...google, services: { ...google.services, llm: svc({ custom: false, models: [{ id: "m", label: "M" }] }) } };
    render(<Harness kind="llm" providers={[strict]} initial={{ provider: "google", model: "m" }} />);
    expect(screen.queryByRole("option", { name: "Other…" })).toBeNull();
  });

  it("names the missing variables when nothing is available", () => {
    render(<Harness kind="llm" providers={[xai]} initial={{ provider: "" }} />);
    expect(screen.getByRole("note")).toHaveTextContent("XAI_API_KEY");
  });

  it("shows the language select only for STT with languages", () => {
    const { unmount } = render(<Harness kind="llm" providers={[google]} initial={{ provider: "google", model: "gemini-flash" }} />);
    expect(screen.queryByLabelText("STT language")).toBeNull();
    unmount();
    render(<Harness kind="stt" providers={[google]} initial={{ provider: "google", model: "chirp_2" }} />);
    expect(screen.getByLabelText("STT language")).toBeInTheDocument();
  });
});
