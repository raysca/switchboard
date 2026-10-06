import { afterEach, describe, expect, it, vi } from "vitest";
import type { ApiClient } from "../api/client";
import type { BrowserAudioController } from "./audio";
import { VoiceClient } from "./VoiceClient";

function deferred<T>() {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((r) => (resolve = r));
  return { promise, resolve };
}

function setup() {
  const token = deferred<{ token: string; expires_at: string; scope: string }>();
  const capture = deferred<void>();
  const api = { issueVoiceToken: vi.fn(() => token.promise) } as unknown as ApiClient;
  const audio = {
    startCapture: vi.fn(() => capture.promise),
    stopCapture: vi.fn(),
    stopPlayback: vi.fn(),
    destroy: vi.fn(),
  } as unknown as BrowserAudioController;
  const sockets = vi.fn();
  vi.stubGlobal("WebSocket", sockets);
  return { client: new VoiceClient(api, audio), token, capture, audio, sockets };
}

describe("VoiceClient connect cancelled by disconnect", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("does not open the mic or socket when disconnected while fetching the token", async () => {
    const { client, token, audio, sockets } = setup();
    const done = client.connect({ agentId: "a1" });
    await client.disconnect();
    token.resolve({ token: "x.e30.y", expires_at: "", scope: "" });
    await done;
    expect(audio.startCapture).not.toHaveBeenCalled();
    expect(sockets).not.toHaveBeenCalled();
    expect(client.getState().sessionState).toBe("disconnected");
  });

  it("releases the mic and opens no socket when disconnected while the mic opens", async () => {
    const { client, token, capture, audio, sockets } = setup();
    const done = client.connect({ agentId: "a1" });
    token.resolve({ token: "x.e30.y", expires_at: "", scope: "" });
    await vi.waitFor(() => expect(audio.startCapture).toHaveBeenCalled());
    await client.disconnect();
    capture.resolve();
    await done;
    expect(audio.stopCapture).toHaveBeenCalled();
    expect(sockets).not.toHaveBeenCalled();
    expect(client.getState().sessionState).toBe("disconnected");
  });
});
