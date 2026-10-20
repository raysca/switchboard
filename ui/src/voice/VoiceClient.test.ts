import { afterEach, describe, expect, it, vi } from "vitest";
import type { ApiClient } from "../api/client";
import { BrowserAudioController } from "./audio";
import { VoiceClient } from "./VoiceClient";

function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
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

  const TOKEN = { token: "x.e30.y", expires_at: "", scope: "" };

  it("asks for the mic first and the token only after it is granted", async () => {
    const { client, token, capture, audio, sockets } = setup();
    const issue = (client as unknown as { apiClient: { issueVoiceToken: ReturnType<typeof vi.fn> } }).apiClient
      .issueVoiceToken;
    const done = client.connect({ flowId: "a1" });
    await vi.waitFor(() => expect(audio.startCapture).toHaveBeenCalled());
    expect(issue).not.toHaveBeenCalled();
    capture.resolve();
    await vi.waitFor(() => expect(issue).toHaveBeenCalledTimes(1));
    token.resolve(TOKEN);
    await done;
    expect(sockets).toHaveBeenCalledTimes(1);
  });

  it("a mic denied asks for no token", async () => {
    const { client, audio } = setup();
    const issue = (client as unknown as { apiClient: { issueVoiceToken: ReturnType<typeof vi.fn> } }).apiClient
      .issueVoiceToken;
    (audio.startCapture as ReturnType<typeof vi.fn>).mockRejectedValue({
      type: "permission_denied",
      message: "Microphone permission was denied.",
    });
    await client.connect({ flowId: "a1" });
    expect(issue).not.toHaveBeenCalled();
    expect(client.getState().error?.category).toBe("mic");
  });

  it("a token failure after the mic releases the mic", async () => {
    const { client, token, capture, audio } = setup();
    const done = client.connect({ flowId: "a1" });
    capture.resolve();
    await vi.waitFor(() => expect(audio.startCapture).toHaveBeenCalled());
    await vi.waitFor(() =>
      expect(
        (client as unknown as { apiClient: { issueVoiceToken: ReturnType<typeof vi.fn> } }).apiClient.issueVoiceToken,
      ).toHaveBeenCalled(),
    );
    token.reject(new Error("nope"));
    await done;
    expect(audio.stopCapture).toHaveBeenCalled();
    expect(client.getState().error?.category).toBe("token");
  });

  it("asks for no token and opens no socket when disconnected while the mic opens", async () => {
    const { client, capture, audio, sockets } = setup();
    const issue = (client as unknown as { apiClient: { issueVoiceToken: ReturnType<typeof vi.fn> } }).apiClient
      .issueVoiceToken;
    const done = client.connect({ flowId: "a1" });
    await vi.waitFor(() => expect(audio.startCapture).toHaveBeenCalled());
    await client.disconnect();
    capture.resolve();
    await done;
    expect(issue).not.toHaveBeenCalled();
    expect(sockets).not.toHaveBeenCalled();
    expect(client.getState().sessionState).toBe("disconnected");
  });

  it("releases the mic and opens no socket when disconnected while the token is fetched", async () => {
    const { client, token, capture, audio, sockets } = setup();
    const done = client.connect({ flowId: "a1" });
    capture.resolve();
    await vi.waitFor(() =>
      expect(
        (client as unknown as { apiClient: { issueVoiceToken: ReturnType<typeof vi.fn> } }).apiClient.issueVoiceToken,
      ).toHaveBeenCalled(),
    );
    await client.disconnect();
    token.resolve(TOKEN);
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

  it("asks for a draft token only when given a draft", async () => {
    const { client, token, capture } = setup();
    const issue = (client as unknown as { apiClient: { issueVoiceToken: ReturnType<typeof vi.fn> } }).apiClient
      .issueVoiceToken;
    capture.resolve();
    token.resolve(TOKEN);
    await client.connect({ flowId: "f", draft: { config: { a: 1 } } });
    await client.disconnect();
    await client.connect({ flowId: "f" });
    expect(issue.mock.calls[0][0]).toMatchObject({ flow_id: "f", draft: { config: { a: 1 } } });
    expect(issue.mock.calls[1][0]).not.toHaveProperty("draft");
  });
});

describe("VoiceClient websocket URL", () => {
  afterEach(() => vi.unstubAllGlobals());
  const resolve = (base?: string) =>
    (new VoiceClient({} as ApiClient, {} as BrowserAudioController) as unknown as {
      resolveWsUrl(token: string, base?: string): string;
    }).resolveWsUrl("tok", base);

  it("derives ws:// from an http page, keeping host and port", () => {
    vi.stubGlobal("window", { location: { protocol: "http:", host: "localhost:8080" } });
    expect(resolve()).toBe("ws://localhost:8080/ws?token=tok");
  });

  it("derives wss:// from an https page", () => {
    vi.stubGlobal("window", { location: { protocol: "https:", host: "voice.example.com" } });
    expect(resolve()).toBe("wss://voice.example.com/ws?token=tok");
  });

  it("has no hardcoded fallback when there is no page location", () => {
    vi.stubGlobal("window", undefined);
    expect(() => resolve()).toThrow();
  });
});
