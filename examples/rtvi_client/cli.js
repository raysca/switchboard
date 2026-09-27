#!/usr/bin/env node

/**
 * cli.js - Standalone Pipecat RTVI Test Driver
 *
 * Connects to Voice Framework V1 server using unmodified @pipecat-ai/client-js
 * and @pipecat-ai/websocket-transport, negotiates RTVI 2.1.0 readiness, streams
 * audio through VAD and STT, receives streamed LLM text and synthesized TTS audio,
 * and reports end-to-end latencies.
 */

const fs = require('fs');
const path = require('path');
const crypto = require('crypto');
const jwt = require('jsonwebtoken');

// Browser polyfills for Node runtime
if (typeof globalThis.window === 'undefined') {
  globalThis.window = {
    addEventListener: () => {},
    removeEventListener: () => {}
  };
}
if (typeof globalThis.navigator === 'undefined') {
  globalThis.navigator = {
    mediaDevices: {
      addEventListener: () => {},
      removeEventListener: () => {}
    }
  };
}

const { WebSocketTransport } = require('@pipecat-ai/websocket-transport');
const { PipecatClient } = require('@pipecat-ai/client-js');

// Parse CLI arguments
const args = process.argv.slice(2);
function getArg(flag, defaultValue) {
  const idx = args.indexOf(flag);
  if (idx !== -1 && idx + 1 < args.length) {
    return args[idx + 1];
  }
  return defaultValue;
}

const pipeline = getArg('--pipeline', 'default'); // "default" (Google) or "xai"
const wsBaseUrl = getArg('--url', 'ws://127.0.0.1:8080/ws');
const audioPath = getArg('--audio', path.resolve(__dirname, '../../assets/speech/clip1.wav'));
const secretKey = getArg('--secret', 'dev-insecure-secret-key-for-testing-only-12345');
let explicitToken = getArg('--token', null);

// Mint admission token if not explicitly provided
if (!explicitToken) {
  const now = Math.floor(Date.now() / 1000);
  explicitToken = jwt.sign(
    {
      jti: crypto.randomUUID(),
      sub: 'rtvi-cli-tester',
      aud: 'govoice-rtvi',
      iss: 'govoice',
      exp: now + 300,
      iat: now,
      pipeline: pipeline
    },
    secretKey,
    { algorithm: 'HS256' }
  );
}

const targetWsUrl = `${wsBaseUrl}?token=${explicitToken}&pipeline=${pipeline}`;

async function main() {
  console.log('================================================================');
  console.log(` Voice Framework V1: Standalone RTVI Client Test Driver`);
  console.log(` Target URL:  ${wsBaseUrl}`);
  console.log(` Pipeline:    ${pipeline} (${pipeline === 'xai' ? 'Google STT -> xAI Grok LLM -> Google TTS' : 'Google STT -> Gemini LLM -> Google TTS'})`);
  console.log(` Audio Input: ${audioPath}`);
  console.log('================================================================\n');

  let botReadyReceived = false;
  let botVersion = null;
  let userStartedSpeaking = false;
  let userStoppedSpeaking = false;
  let userTranscript = '';
  let botLlmText = '';
  let botStartedSpeaking = false;
  let botStoppedSpeaking = false;
  let botAudioFrames = 0;
  let botAudioBytes = 0;
  let userAudioCallback = null;
  let fatalError = null;

  // Latency markers
  const t0_connect = Date.now();
  let t_bot_ready = 0;
  let t_speech_start = 0;
  let t_transcript = 0;
  let t_first_llm_token = 0;
  let t_first_audio = 0;

  const mockMediaManager = {
    initialize: async () => {},
    connect: async () => {},
    disconnect: async () => {},
    setUserAudioCallback: (cb) => {
      userAudioCallback = cb;
    },
    setClientOptions: () => {},
    tracks: () => ({}),
    userStartedSpeaking: async () => {},
    bufferBotAudio: (data) => {
      botAudioFrames++;
      botAudioBytes += data ? (data.byteLength || data.length || 0) : 0;
      if (!t_first_audio) {
        t_first_audio = Date.now();
        console.log(`\n[BOT AUDIO] First audio frame received! (+${t_first_audio - t0_connect}ms from start)`);
      }
    }
  };

  const transport = new WebSocketTransport({ mediaManager: mockMediaManager });
  const client = new PipecatClient({
    transport: transport,
    enableMic: false,
    enableCam: false,
    callbacks: {
      onBotReady: (data) => {
        botReadyReceived = true;
        botVersion = data?.version;
        t_bot_ready = Date.now();
        console.log(`[HANDSHAKE] Bot ready! Protocol version: ${botVersion} (took ${t_bot_ready - t0_connect}ms)`);
      },
      onUserStartedSpeaking: () => {
        userStartedSpeaking = true;
        t_speech_start = Date.now();
        console.log(`[VAD] User started speaking (+${t_speech_start - t0_connect}ms)`);
      },
      onUserStoppedSpeaking: () => {
        userStoppedSpeaking = true;
        console.log(`[VAD] User stopped speaking (+${Date.now() - t0_connect}ms)`);
      },
      onUserTranscript: (data) => {
        if (data?.final) {
          userTranscript = data.text;
          t_transcript = Date.now();
          console.log(`[STT FINAL] "${userTranscript}" (+${t_transcript - t0_connect}ms)`);
        } else if (data?.text) {
          process.stdout.write(`\r[STT INTERIM] ${data.text}      `);
        }
      },
      onBotLlmText: (data) => {
        if (!t_first_llm_token) {
          t_first_llm_token = Date.now();
          process.stdout.write(`\n[LLM STREAM] `);
        }
        if (data?.text) {
          botLlmText += data.text;
          process.stdout.write(data.text);
        }
      },
      onBotStartedSpeaking: () => {
        botStartedSpeaking = true;
        console.log(`\n[BOT STATE] Bot started speaking`);
      },
      onBotStoppedSpeaking: () => {
        botStoppedSpeaking = true;
        console.log(`[BOT STATE] Bot stopped speaking`);
      },
      onError: (err) => {
        console.error(`\n[ERROR] Received error:`, err);
        fatalError = err;
      }
    }
  });

  // Step 1: Connect
  console.log(`Connecting to server...`);
  try {
    await client.connect({ wsUrl: targetWsUrl });
  } catch (err) {
    console.error(`Failed to connect to ${targetWsUrl}:`, err.message);
    process.exit(1);
  }

  // Wait for bot-ready
  const waitReadyStart = Date.now();
  while (!botReadyReceived && Date.now() - waitReadyStart < 5000) {
    await new Promise(r => setTimeout(r, 50));
  }
  if (!botReadyReceived) {
    console.error('Timed out waiting for bot-ready');
    process.exit(1);
  }

  // Step 2: Stream audio file if available
  if (fs.existsSync(audioPath) && userAudioCallback) {
    console.log(`\nStreaming speech audio file (${audioPath})...`);
    // Extract raw PCM audio from WAV data chunk if RIFF
    const fileBytes = fs.readFileSync(audioPath);
    let rawAudio = fileBytes;
    if (fileBytes.length > 12 && fileBytes.toString('ascii', 0, 4) === 'RIFF') {
      const dataIdx = fileBytes.indexOf(Buffer.from('data'));
      if (dataIdx !== -1) {
        const len = fileBytes.readUInt32LE(dataIdx + 4);
        rawAudio = fileBytes.slice(dataIdx + 8, dataIdx + 8 + len);
      }
    }

    const chunkSize = 640; // 20ms @ 16kHz 16-bit mono = 320 samples = 640 bytes
    let offset = 0;

    // Send 100ms silence leading buffer
    for (let i = 0; i < 5; i++) {
      userAudioCallback(new Int16Array(320).buffer);
      await new Promise(r => setTimeout(r, 20));
    }

    // Stream audio at real-time 20ms cadence
    while (offset < rawAudio.length) {
      const end = Math.min(offset + chunkSize, rawAudio.length);
      const chunk = rawAudio.slice(offset, end);
      const aligned = new Uint8Array(chunkSize);
      aligned.set(chunk);
      userAudioCallback(aligned.buffer);
      offset += chunkSize;
      await new Promise(r => setTimeout(r, 20));
    }

    // Send trailing silence to trigger speech stop (50 chunks * 20ms = 1000ms > StopChunks 25*32ms)
    console.log(`Audio stream finished. Sending trailing silence...`);
    for (let i = 0; i < 50; i++) {
      userAudioCallback(new Int16Array(320).buffer);
      await new Promise(r => setTimeout(r, 20));
    }
  } else {
    console.log(`No audio file found or userAudioCallback unavailable; sending synthetic audio tone...`);
    // Send 1 second of synthetic speech
    for (let i = 0; i < 50; i++) {
      const tone = new Int16Array(320);
      for (let s = 0; s < 320; s++) {
        tone[s] = Math.sin((s + i * 320) * 0.1) * 10000;
      }
      userAudioCallback(tone.buffer);
      await new Promise(r => setTimeout(r, 20));
    }
    // Trailing silence
    for (let i = 0; i < 50; i++) {
      userAudioCallback(new Int16Array(320).buffer);
      await new Promise(r => setTimeout(r, 20));
    }
  }

  // Step 3: Wait for assistant response and audio
  console.log(`\nWaiting for assistant response...`);
  const waitResponseStart = Date.now();
  while (Date.now() - waitResponseStart < 20000) {
    if (botLlmText && botAudioFrames > 0 && botStoppedSpeaking) {
      break;
    }
    await new Promise(r => setTimeout(r, 100));
  }

  // Clean disconnect
  console.log(`\nTearing down call...`);
  await client.disconnect();

  console.log('\n================================================================');
  console.log(` Call Test Results Summary: [${pipeline.toUpperCase()}]`);
  console.log('================================================================');
  console.log(` Handshake Success:       ${botReadyReceived ? 'PASS (' + (t_bot_ready - t0_connect) + 'ms)' : 'FAIL'}`);
  console.log(` User Speech Detected:    ${userStartedSpeaking ? 'PASS' : 'SKIPPED'}`);
  console.log(` User Transcript:         ${userTranscript ? '"' + userTranscript + '"' : '(none)'}`);
  console.log(` Bot LLM Response:        ${botLlmText ? '"' + botLlmText.trim() + '"' : '(none)'}`);
  console.log(` Bot Audio Synthesized:   ${botAudioFrames > 0 ? 'PASS (' + botAudioFrames + ' frames, ' + botAudioBytes + ' bytes)' : 'FAIL'}`);
  if (t_transcript && t_first_llm_token) {
    console.log(` STT -> First LLM Token:  ${t_first_llm_token - t_transcript} ms`);
  }
  if (t_transcript && t_first_audio) {
    console.log(` STT -> First Audio Out:  ${t_first_audio - t_transcript} ms`);
  }
  console.log('================================================================\n');

  if (fatalError) {
    console.error(`Call completed with fatal error:`, fatalError);
    process.exit(1);
  }
  process.exit(0);
}

main().catch(err => {
  console.error(`Fatal client execution error:`, err);
  process.exit(1);
});
