import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";
import {
  decodeBinaryFrame,
  decodeVarint,
  encodeAudioFrame,
  encodeClientReadyFrame,
  encodeDisconnectFrame,
  encodeMessageFrame,
  encodeSetMutedFrame,
  encodeVarint,
  extractSessionIdFromToken,
  RTVI_LABEL,
  RTVI_TYPE_BOT_READY,
  RTVI_TYPE_CLIENT_READY,
  RTVI_TYPE_DISCONNECT,
  RTVI_TYPE_SET_MUTED,
} from "./protocol";

describe("RTVI Protobuf Wire Protocol", () => {
  it("encodes and decodes varints correctly", () => {
    const values = [0, 1, 127, 128, 300, 16000, 24000, 65535, 100000];
    for (const val of values) {
      const encoded = encodeVarint(val);
      const [decoded, bytesRead] = decodeVarint(new Uint8Array(encoded), 0);
      expect(decoded).toBe(val);
      expect(bytesRead).toBe(encoded.length);
    }
  });

  it("encodes and decodes message frames round-trip", () => {
    const json = JSON.stringify({
      id: "test-1",
      label: RTVI_LABEL,
      type: "user-transcription",
      data: { text: "Hello world", final: true },
    });
    const bytes = encodeMessageFrame(json);
    const decoded = decodeBinaryFrame(bytes);
    expect(decoded).not.toBeNull();
    expect(decoded?.kind).toBe("message");
    if (decoded?.kind === "message") {
      expect(decoded.data).toBe(json);
      expect(decoded.parsed?.type).toBe("user-transcription");
      expect((decoded.parsed?.data as { text: string }).text).toBe("Hello world");
    }
  });

  it("encodes client-ready and disconnect frames with standard types", () => {
    const readyBytes = encodeClientReadyFrame("2.1.0");
    const readyDecoded = decodeBinaryFrame(readyBytes);
    expect(readyDecoded?.kind).toBe("message");
    if (readyDecoded?.kind === "message") {
      expect(readyDecoded.parsed?.type).toBe(RTVI_TYPE_CLIENT_READY);
      expect((readyDecoded.parsed?.data as { version: string }).version).toBe("2.1.0");
    }

    const discBytes = encodeDisconnectFrame();
    const discDecoded = decodeBinaryFrame(discBytes);
    expect(discDecoded?.kind).toBe("message");
    if (discDecoded?.kind === "message") {
      expect(discDecoded.parsed?.type).toBe(RTVI_TYPE_DISCONNECT);
    }
  });

  it("encodes set-muted with the muted flag", () => {
    const decoded = decodeBinaryFrame(encodeSetMutedFrame(true));
    expect(decoded?.kind === "message" && decoded.parsed).toMatchObject({
      label: RTVI_LABEL,
      type: RTVI_TYPE_SET_MUTED,
      data: { muted: true },
    });
  });

  it("encodes and decodes audio frames round-trip", () => {
    const samples = new Uint8Array(640); // 320 samples (20ms at 16kHz)
    for (let i = 0; i < samples.length; i++) {
      samples[i] = i % 256;
    }
    const audioBytes = encodeAudioFrame(samples, 16000, 1);
    const decoded = decodeBinaryFrame(audioBytes);
    expect(decoded?.kind).toBe("audio");
    if (decoded?.kind === "audio") {
      expect(decoded.sampleRate).toBe(16000);
      expect(decoded.audio.length).toBe(640);
      expect(decoded.audio[5]).toBe(5);
    }
  });

  it("decodes nested-rtvi.bin fixture matching Go server output", () => {
    const fixturePath = path.resolve(__dirname, "../../../tools/compat/fixtures/nested-rtvi.bin");
    if (fs.existsSync(fixturePath)) {
      const fixtureBytes = new Uint8Array(fs.readFileSync(fixturePath));
      const decoded = decodeBinaryFrame(fixtureBytes);
      expect(decoded?.kind).toBe("message");
      if (decoded?.kind === "message") {
        expect(decoded.parsed?.label).toBe("rtvi-ai");
        expect(decoded.parsed?.type).toBe(RTVI_TYPE_BOT_READY);
        expect((decoded.parsed?.data as { version: string }).version).toBe("2.1.0");
      }
    }
  });

  it("decodes client-audio.bin fixture correctly", () => {
    const fixturePath = path.resolve(__dirname, "../../../tools/compat/fixtures/client-audio.bin");
    if (fs.existsSync(fixturePath)) {
      const fixtureBytes = new Uint8Array(fs.readFileSync(fixturePath));
      const decoded = decodeBinaryFrame(fixtureBytes);
      expect(decoded?.kind).toBe("audio");
      if (decoded?.kind === "audio") {
        expect(decoded.sampleRate).toBe(16000);
        expect(decoded.audio.length).toBe(640); // 320 PCM16 samples
      }
    }
  });

  it("decodes server-audio.bin fixture correctly", () => {
    const fixturePath = path.resolve(__dirname, "../../../tools/compat/fixtures/server-audio.bin");
    if (fs.existsSync(fixturePath)) {
      const fixtureBytes = new Uint8Array(fs.readFileSync(fixturePath));
      const decoded = decodeBinaryFrame(fixtureBytes);
      expect(decoded?.kind).toBe("audio");
      if (decoded?.kind === "audio") {
        expect(decoded.sampleRate).toBe(24000);
        expect(decoded.audio.length).toBe(960); // 480 PCM16 samples @ 24kHz
      }
    }
  });

  it("rejects truncated or malformed buffers safely", () => {
    const malformed = new Uint8Array([0x22, 0xff, 0x01]); // Length exceeds buffer
    expect(() => decodeBinaryFrame(malformed)).toThrow(/Malformed protobuf/);
  });

  it("extracts session ID from admission JWT token", () => {
    // Header: {"alg":"HS256","typ":"JWT"} -> eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9
    // Payload: {"jti":"session-uuid-12345","sub":"alice","pipeline":"default"} -> eyJqdGkiOiJzZXNzaW9uLXV1aWQtMTIzNDUiLCJzdWIiOiJhbGljZSIsInBpcGVsaW5lIjoiZGVmYXVsdCJ9
    const dummyJwt =
      "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJqdGkiOiJzZXNzaW9uLXV1aWQtMTIzNDUiLCJzdWIiOiJhbGljZSIsInBpcGVsaW5lIjoiZGVmYXVsdCJ9.signature";
    const sessionId = extractSessionIdFromToken(dummyJwt);
    expect(sessionId).toBe("session-uuid-12345");

    expect(extractSessionIdFromToken("invalid")).toBeNull();
  });
});
