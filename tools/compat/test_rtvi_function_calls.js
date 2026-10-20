/**
 * test_rtvi_function_calls.js
 *
 * The released @pipecat-ai/client-js@1.13.1 with @pipecat-ai/websocket-transport@1.7.2, against a running
 * Switchboard server: one text turn makes the flow call a tool; prints, as one JSON line after "EVENTS ",
 * the function-call callbacks the client fired (in order), the ids its event emitter saw and the bot-tts-text payloads.
 * Driven by server/tool_events_client_e2e_test.go, which asserts them. A
 * second "barrier" turn is answered before the events are printed, so the list is complete.
 *
 * Usage: node test_rtvi_function_calls.js --url <wsUrl> --token <token> --text <message>
 */
if (typeof globalThis.window === 'undefined') {
  globalThis.window = { addEventListener: () => {}, removeEventListener: () => {} };
}
if (typeof globalThis.navigator === 'undefined') {
  globalThis.navigator = { mediaDevices: { addEventListener: () => {}, removeEventListener: () => {} } };
}

const { WebSocketTransport } = require('@pipecat-ai/websocket-transport');
const { PipecatClient } = require('@pipecat-ai/client-js');

function arg(name) {
  const i = process.argv.indexOf(name);
  if (i === -1 || i + 1 >= process.argv.length) throw new Error(`missing ${name}`);
  return process.argv[i + 1];
}

function withTimeout(promise, ms, what) {
  return Promise.race([promise, new Promise((_, reject) => {
    const t = setTimeout(() => reject(new Error(`${what} timed out after ${ms}ms`)), ms);
    if (t.unref) t.unref();
  })]);
}

async function main() {
  const events = [];
  const emitted = [];
  const tts = [];
  const errors = [];
  let replied = false;
  let barrierAnswered = false;
  const mediaManager = {
    initialize: async () => {}, connect: async () => {}, disconnect: async () => {},
    setUserAudioCallback: () => {}, setClientOptions: () => {}, tracks: () => ({}),
    userStartedSpeaking: async () => {}, userStoppedSpeaking: async () => {}, bufferBotAudio: () => {},
  };
  const client = new PipecatClient({
    transport: new WebSocketTransport({ mediaManager }),
    enableMic: false,
    enableCam: false,
    callbacks: {
      onLLMFunctionCallStarted: (data) => events.push({ type: 'started', data }),
      onLLMFunctionCallInProgress: (data) => events.push({ type: 'in-progress', data }),
      onLLMFunctionCallStopped: (data) => events.push({ type: 'stopped', data }),
      onBotLlmText: (data) => {
        replied = true;
        if (data?.text?.includes('barrier')) barrierAnswered = true;
      },
      // bot-tts-text follows the unit's audio, after bot-llm-text: its payload is {text}.
      onBotTtsText: (data) => tts.push(data?.text ?? ''),
      onError: (err) => errors.push(err),
    },
  });
  // RTVIEvent.LLMFunctionCallInProgress is "llmFunctionCallInProgress" in the pinned dist (index.js:957).
  client.on('llmFunctionCallInProgress', (data) => emitted.push(data.tool_call_id));
  await withTimeout(client.connect({ wsUrl: arg('--url'), token: arg('--token') }), 5000, 'connect');
  await client.sendText(arg('--text'));
  const until = Date.now() + 8000;
  while (Date.now() < until && !(replied && events.some((e) => e.type === 'stopped'))) {
    await new Promise((r) => setTimeout(r, 20));
  }
  // Ordering barrier: once a later turn is answered, a late or duplicate event would have arrived too.
  // (A text turn sent while the previous reply is still committing can be lost: resend, at most 3 times.)
  for (let i = 0; i < 3 && !barrierAnswered; i++) {
    await client.sendText('barrier');
    const by = Date.now() + 3000;
    while (Date.now() < by && !barrierAnswered) await new Promise((r) => setTimeout(r, 20));
  }
  if (!barrierAnswered) throw new Error('the barrier turn was never answered');
  // The spoken text of the barrier reply follows its audio (and bot-llm-text, which set barrierAnswered).
  for (const by = Date.now() + 5000; Date.now() < by && !tts.some((s) => s.includes('barrier'));) {
    await new Promise((r) => setTimeout(r, 20));
  }
  await client.disconnect();
  if (errors.length) throw new Error(`RTVI errors: ${JSON.stringify(errors)}`);
  console.log('EVENTS ' + JSON.stringify({ events, emitted, tts }));
}

main().then(() => process.exit(0)).catch((err) => {
  console.error('FAILED:', err);
  process.exit(1);
});
