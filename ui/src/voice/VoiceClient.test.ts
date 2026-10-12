import { afterEach, describe, expect, it, vi } from "vitest";
import type { ApiClient } from "../api/client";
import { BrowserAudioController } from "./audio";
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
    const done = client.connect({ flowId: "a1" });
    await client.disconnect();
    token.resolve({ token: "x.e30.y", expires_at: "", scope: "" });
    await done;
    expect(audio.startCapture).not.toHaveBeenCalled();
    expect(sockets).not.toHaveBeenCalled();
    expect(client.getState().sessionState).toBe("disconnected");
  });

  it("releases the mic and opens no socket when disconnected while the mic opens", async () => {
    const { client, token, capture, audio, sockets } = setup();
    const done = client.connect({ flowId: "a1" });
    token.resolve({ token: "x.e30.y", expires_at: "", scope: "" });
    await vi.waitFor(() => expect(audio.startCapture).toHaveBeenCalled());
    await client.disconnect();
    capture.resolve();
    await done;
    expect(audio.destroy).toHaveBeenCalled(); // disconnect released the mic
    expect(sockets).not.toHaveBeenCalled();
    expect(client.getState().sessionState).toBe("disconnected");
  });

  it("an abandoned attempt does not stop the next attempt's mic", async () => {
    const gum = [deferred<MediaStream>(), deferred<MediaStream>()];
    let calls = 0;
    const tracks: { stopped: boolean }[] = [];
    const stream = () => {
      const t = { stopped: false, stop() { this.stopped = true; } };
      tracks.push(t);
      return { getTracks: () => [t], getAudioTracks: () => [t] } as unknown as MediaStream;
    };
    vi.stubGlobal("navigator", { mediaDevices: { getUserMedia: vi.fn(() => gum[calls++].promise) } });
    class FakeAudioContext {
      state = "running";
      destination = {};
      createMediaStreamSource() { return { connect() {}, disconnect() {} }; }
      createScriptProcessor() { return { connect() {}, disconnect() {}, onaudioprocess: null }; }
      close() { return Promise.resolve(); }
      resume() { return Promise.resolve(); }
    }
    vi.stubGlobal("AudioContext", FakeAudioContext);
    const sockets: string[] = [];
    vi.stubGlobal("WebSocket", class { static OPEN = 1; readyState = 0; binaryType = ""; constructor(u: string) { sockets.push(u); } close() {} send() {} });
    const api = { issueVoiceToken: vi.fn(async () => ({ token: "x.e30.y", expires_at: "", scope: "" })) } as unknown as ApiClient;
    const audio = new BrowserAudioController();
    const client = new VoiceClient(api, audio);

    const first = client.connect({ flowId: "a", wsBaseUrl: "ws://h" });
    await vi.waitFor(() => expect(calls).toBe(1));
    await client.disconnect();
    const second = client.connect({ flowId: "a", wsBaseUrl: "ws://h" });
    await vi.waitFor(() => expect(calls).toBe(2));
    gum[0].resolve(stream());
    await new Promise((r) => setTimeout(r, 0));
    gum[1].resolve(stream());
    await first;
    await second;

    expect(tracks[0].stopped).toBe(true); // the abandoned mic is released
    expect(tracks[1].stopped).toBe(false); // the live attempt keeps its mic
    expect(sockets).toHaveLength(1);
  });
});
