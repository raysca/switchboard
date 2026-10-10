/**
 * RTVI and Protobuf Wire Protocol Implementation.
 * Compatible with @pipecat-ai/client-js@1.13.1 and @pipecat-ai/websocket-transport@1.7.2.
 */

export const RTVI_LABEL = "rtvi-ai";
export const RTVI_PROTOCOL_VERSION = "2.1.0";

export const RTVI_TYPE_CLIENT_READY = "client-ready";
export const RTVI_TYPE_BOT_READY = "bot-ready";
export const RTVI_TYPE_USER_STARTED_SPEAKING = "user-started-speaking";
export const RTVI_TYPE_USER_STOPPED_SPEAKING = "user-stopped-speaking";
export const RTVI_TYPE_USER_TRANSCRIPTION = "user-transcription";
export const RTVI_TYPE_BOT_LLM_TEXT = "bot-llm-text";
export const RTVI_TYPE_BOT_STARTED_SPEAKING = "bot-started-speaking";
export const RTVI_TYPE_BOT_STOPPED_SPEAKING = "bot-stopped-speaking";
export const RTVI_TYPE_ERROR = "error";
export const RTVI_TYPE_DISCONNECT = "disconnect-bot";
export const RTVI_TYPE_SEND_TEXT = "send-text";
export const RTVI_TYPE_SET_MUTED = "set-muted";

export interface RTVIMessage<T = unknown> {
  id?: string;
  label: string;
  type: string;
  data?: T;
}

export interface RTVIClientReadyData {
  version: string;
  about?: Record<string, unknown>;
}

export interface RTVITranscriptionData {
  text: string;
  final: boolean;
  timestamp?: string;
  user_id?: string;
}

export interface RTVIBotTextData {
  text: string;
}

export interface RTVIErrorData {
  error: string;
  fatal: boolean;
}

export type DecodedFrame =
  | { kind: "message"; data: string; parsed?: RTVIMessage }
  | { kind: "audio"; audio: Uint8Array; sampleRate: number };

// --- Varint Utilities ---

export function encodeVarint(value: number): number[] {
  const bytes: number[] = [];
  let v = Math.floor(value);
  while (v >= 0x80) {
    bytes.push((v & 0x7f) | 0x80);
    v = Math.floor(v / 128);
  }
  bytes.push(v & 0x7f);
  return bytes;
}

export function decodeVarint(
  bytes: Uint8Array,
  offset: number,
): [number, number] {
  let result = 0;
  let shift = 0;
  let bytesRead = 0;
  while (offset + bytesRead < bytes.length) {
    const b = bytes[offset + bytesRead];
    bytesRead++;
    result += (b & 0x7f) * Math.pow(2, shift);
    shift += 7;
    if ((b & 0x80) === 0) {
      break;
    }
    if (bytesRead > 10) {
      throw new Error("Varint too long");
    }
  }
  return [result, bytesRead];
}

// --- Protobuf Frame Encoders ---

const textEncoder = new TextEncoder();
const textDecoder = new TextDecoder();

/**
 * Packages a string payload (e.g. JSON RTVI message) into a protobuf binary Frame_Message.
 */
export function encodeMessageFrame(data: string): Uint8Array {
  const dataBytes = textEncoder.encode(data);
  const dataLenVarint = encodeVarint(dataBytes.length);

  // MessageFrame: field 1 (data), wire type 2 -> tag 0x0A (10)
  const messageFrameBytes = new Uint8Array(
    1 + dataLenVarint.length + dataBytes.length,
  );
  messageFrameBytes[0] = 0x0a;
  messageFrameBytes.set(dataLenVarint, 1);
  messageFrameBytes.set(dataBytes, 1 + dataLenVarint.length);

  // Frame: field 4 (message), wire type 2 -> tag 0x22 (34)
  const frameLenVarint = encodeVarint(messageFrameBytes.length);
  const result = new Uint8Array(
    1 + frameLenVarint.length + messageFrameBytes.length,
  );
  result[0] = 0x22;
  result.set(frameLenVarint, 1);
  result.set(messageFrameBytes, 1 + frameLenVarint.length);

  return result;
}

/**
 * Formats an RTVI JSON message and packages it into a binary protobuf Frame.
 */
export function encodeRTVIMessage<T = unknown>(
  type: string,
  data?: T,
  id?: string,
): Uint8Array {
  const msg: RTVIMessage<T> = {
    id: id ?? `client-msg-${Date.now()}-${Math.floor(Math.random() * 1000)}`,
    label: RTVI_LABEL,
    type,
    ...(data !== undefined ? { data } : {}),
  };
  return encodeMessageFrame(JSON.stringify(msg));
}

/**
 * Encodes the standard RTVI client-ready handshake frame.
 */
export function encodeClientReadyFrame(
  version: string = RTVI_PROTOCOL_VERSION,
): Uint8Array {
  return encodeRTVIMessage<RTVIClientReadyData>(RTVI_TYPE_CLIENT_READY, {
    version,
  });
}

/**
 * Encodes the standard RTVI disconnect-bot frame.
 */
export function encodeDisconnectFrame(): Uint8Array {
  return encodeRTVIMessage(RTVI_TYPE_DISCONNECT);
}

/**
 * Encodes an RTVI send-text frame: a typed user turn the server treats like a final transcription.
 */
export function encodeSendTextFrame(content: string): Uint8Array {
  return encodeRTVIMessage(RTVI_TYPE_SEND_TEXT, { content });
}

/** Encodes set-muted: the server drops the caller's audio while muted. */
export function encodeSetMutedFrame(muted: boolean): Uint8Array {
  return encodeRTVIMessage(RTVI_TYPE_SET_MUTED, { muted });
}

/**
 * Packages raw PCM audio samples into a binary protobuf Frame_Audio.
 */
export function encodeAudioFrame(
  samples: Uint8Array,
  sampleRate: number = 16000,
  numChannels: number = 1,
): Uint8Array {
  const audioLenVarint = encodeVarint(samples.length);
  const rateVarint = encodeVarint(sampleRate);
  const chanVarint = encodeVarint(numChannels);

  // AudioRawFrame fields:
  // field 2 (name = "audio"): tag 0x12, length 5, bytes "audio"
  const nameBytes = textEncoder.encode("audio");
  const nameLenVarint = encodeVarint(nameBytes.length);

  // field 3 (audio): tag 0x1A
  // field 4 (sample_rate): tag 0x20
  // field 5 (num_channels): tag 0x28
  const innerLen =
    1 +
    nameLenVarint.length +
    nameBytes.length +
    1 +
    audioLenVarint.length +
    samples.length +
    1 +
    rateVarint.length +
    1 +
    chanVarint.length;

  const audioRawBytes = new Uint8Array(innerLen);
  let p = 0;

  // Name
  audioRawBytes[p++] = 0x12;
  audioRawBytes.set(nameLenVarint, p);
  p += nameLenVarint.length;
  audioRawBytes.set(nameBytes, p);
  p += nameBytes.length;

  // Audio bytes
  audioRawBytes[p++] = 0x1a;
  audioRawBytes.set(audioLenVarint, p);
  p += audioLenVarint.length;
  audioRawBytes.set(samples, p);
  p += samples.length;

  // Sample rate
  audioRawBytes[p++] = 0x20;
  audioRawBytes.set(rateVarint, p);
  p += rateVarint.length;

  // Channels
  audioRawBytes[p++] = 0x28;
  audioRawBytes.set(chanVarint, p);
  p += chanVarint.length;

  // Frame: field 2 (audio), wire type 2 -> tag 0x12 (18)
  const outerLenVarint = encodeVarint(audioRawBytes.length);
  const result = new Uint8Array(
    1 + outerLenVarint.length + audioRawBytes.length,
  );
  result[0] = 0x12;
  result.set(outerLenVarint, 1);
  result.set(audioRawBytes, 1 + outerLenVarint.length);

  return result;
}

// --- Protobuf Frame Decoders ---

/**
 * Reads one protobuf message's fields: length-delimited ones as bytes,
 * varints as numbers. Fixed-width fields are skipped.
 */
function readFields(bytes: Uint8Array): Map<number, Uint8Array | number> {
  const fields = new Map<number, Uint8Array | number>();
  let offset = 0;
  while (offset < bytes.length) {
    const [tag, tagBytes] = decodeVarint(bytes, offset);
    offset += tagBytes;
    const wireType = tag & 0x07;
    if (wireType === 2) {
      const [len, lenBytes] = decodeVarint(bytes, offset);
      offset += lenBytes;
      if (offset + len > bytes.length) {
        throw new Error("Malformed protobuf: field length exceeds buffer bounds");
      }
      fields.set(tag >> 3, bytes.subarray(offset, offset + len));
      offset += len;
    } else if (wireType === 0) {
      const [val, valBytes] = decodeVarint(bytes, offset);
      offset += valBytes;
      fields.set(tag >> 3, val);
    } else if (wireType === 1) {
      offset += 8;
    } else if (wireType === 5) {
      offset += 4;
    } else {
      throw new Error(`Unsupported wire type: ${wireType}`);
    }
  }
  return fields;
}

/**
 * Decodes a binary WebSocket message into a DecodedFrame. The server only
 * sends Frame.audio (field 2) and Frame.message (field 4).
 */
export function decodeBinaryFrame(buffer: Uint8Array): DecodedFrame | null {
  const frame = readFields(buffer);
  const message = frame.get(4);
  if (message instanceof Uint8Array) {
    // MessageFrame { string data = 1; }
    const data = readFields(message).get(1);
    const json = data instanceof Uint8Array ? textDecoder.decode(data) : "";
    let parsed: RTVIMessage | undefined;
    try {
      const obj = json ? JSON.parse(json) : null;
      if (obj && typeof obj === "object" && obj.label === RTVI_LABEL) parsed = obj as RTVIMessage;
    } catch {
      // Not JSON: keep the raw string
    }
    return { kind: "message", data: json, parsed };
  }
  const audio = frame.get(2);
  if (audio instanceof Uint8Array) {
    // AudioRawFrame { bytes audio = 3; uint32 sample_rate = 4; }
    const fields = readFields(audio);
    const samples = fields.get(3);
    const rate = fields.get(4);
    return {
      kind: "audio",
      audio: samples instanceof Uint8Array ? new Uint8Array(samples) : new Uint8Array(0),
      sampleRate: typeof rate === "number" ? rate : 24000,
    };
  }
  return null;
}

/**
 * Extracts session ID or token ID (JTI) from a JWT admission token.
 */
export function extractSessionIdFromToken(token: string): string | null {
  try {
    const parts = token.split(".");
    if (parts.length < 2) return null;
    const base64 = parts[1].replace(/-/g, "+").replace(/_/g, "/");
    const jsonStr = atob(base64);
    const payload = JSON.parse(jsonStr);
    return payload.jti || payload.sub || null;
  } catch {
    return null;
  }
}
