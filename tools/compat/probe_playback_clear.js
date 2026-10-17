/**
 * probe_playback_clear.js
 *
 * The hard playback-clear probe, against the exact unmodified released packages
 * @pipecat-ai/client-js@1.13.1 and @pipecat-ai/websocket-transport@1.7.2.
 *
 * Every wire signal a server could send at barge-in is delivered through the real
 * WebSocketTransport message listener (installed by initializeWebsocket(): deserialize, then
 * bufferBotAudio or PipecatClient.handleMessage). The media manager is a Proxy that records every
 * method the transport or the client calls on it; playback is cleared only by userStartedSpeaking()
 * (WavMediaManager: _wavStreamPlayer.interrupt()). Every console.error and console.warn line is
 * recorded too, so a client that fails differently does not match the recording.
 *
 * The observed facts must equal the ones recorded under "probes" in
 * docs/compatibility/playback-clear-decision.json: a pinned client that behaves differently fails
 * `make verify` until the decision is re-recorded from this output.
 */
if (typeof globalThis.window === 'undefined') {
  globalThis.window = { addEventListener: () => {}, removeEventListener: () => {} };
}
if (typeof globalThis.navigator === 'undefined') {
  globalThis.navigator = { mediaDevices: { addEventListener: () => {}, removeEventListener: () => {} } };
}

const fs = require('fs');
const path = require('path');

const { ProtobufFrameSerializer, WebSocketTransport } = require('@pipecat-ai/websocket-transport');
const { PipecatClient } = require('@pipecat-ai/client-js');

const DECISION_PATH = path.join(__dirname, '../../docs/compatibility/playback-clear-decision.json');
const FAILED_VERDICT = 'UNMODIFIED_CLIENT_CANNOT_CLEAR_BUFFERED_PLAYBACK_ON_WIRE_EVENT';
const CLEARS_VERDICT = 'PINNED_CLIENT_CLEARS_BUFFERED_PLAYBACK';
const RELAX_DECISION = 'relax-unmodified-client-playback-clear';

// Each signal's bytes as a server puts them on the socket.
const SIGNALS = {
  // Pipecat's ProtobufFrameSerializer sends InterruptionFrame as Frame.interruption (field 5, empty).
  protobuf_interruption_field_5: () => new Uint8Array([0x2a, 0x00]),
  // Pipecat's RTVIObserver sends this on every InterruptionFrame; Switchboard on a barge-in that cut a reply.
  rtvi_bot_interrupted: (s) => s.serializeMessage({ id: 'probe-1', label: 'rtvi-ai', type: 'bot-interrupted' }),
  rtvi_user_started_speaking: (s) => s.serializeMessage({ id: 'probe-2', label: 'rtvi-ai', type: 'user-started-speaking' }),
};

const tick = () => new Promise((r) => setTimeout(r, 10));

async function probeSignal(bytes) {
  const calls = [];
  const logged = [];
  let delivered = false;
  const mediaManager = new Proxy({}, {
    get: (_, name) => {
      if (typeof name !== 'string' || name === 'then') return undefined;
      return (...args) => {
        calls.push(name);
        return name === 'tracks' ? {} : Promise.resolve();
      };
    },
  });
  const transport = new WebSocketTransport({ mediaManager, wsUrl: 'ws://127.0.0.1:9' });
  new PipecatClient({ transport, callbacks: {} }); // makes handleMessage the transport's message handler
  const ws = transport.initializeWebsocket(); // the socket wrapper and its listeners; nothing is dialed
  calls.length = 0; // setUserAudioCallback/setClientOptions at construction are not reactions to the signal
  const debug = console.debug;
  const error = console.error;
  const warn = console.warn;
  console.debug = (...a) => { if (a[0] === '[RTVI Message]') delivered = true; };
  console.error = (...a) => logged.push('error: ' + a.map(String).join(' '));
  console.warn = (...a) => logged.push('warn: ' + a.map(String).join(' '));
  try {
    ws.emit('message', new Blob([bytes]));
    // The listener awaits blob.arrayBuffer(): wait until it delivered or logged, then once more for a clear.
    for (let i = 0; i < 100 && !delivered && logged.length === 0; i++) await tick();
    await tick();
  } finally {
    console.debug = debug;
    console.error = error;
    console.warn = warn;
  }
  return {
    delivered_to_client: delivered,
    media_manager_calls: calls,
    logged,
  };
}

async function runPlaybackClearProbe() {
  console.log('=== Playback-clear probe (pinned unmodified client) ===');
  const serializer = new ProtobufFrameSerializer();
  const observed = {};
  for (const [name, make] of Object.entries(SIGNALS)) {
    observed[name] = await probeSignal(make(serializer));
    console.log(name + ': ' + JSON.stringify(observed[name]));
  }
  const cleared = Object.values(observed).some((p) => p.media_manager_calls.includes('userStartedSpeaking'));
  const verdict = cleared ? CLEARS_VERDICT : FAILED_VERDICT;
  console.log('verdict: ' + verdict);
  const decision = readProductDecision(verdict, observed);
  if (!decision.ok) {
    console.error(decision.reason);
    console.error('Playback-clear gate failed: re-record docs/compatibility/playback-clear-decision.json from this output.');
    process.exitCode = 2;
    return { verdict, observed };
  }
  if (!cleared) {
    console.error('PLAYBACK_CLEAR_REQUIREMENT_FAILED: pinned unmodified client cannot clear buffered playback.');
    console.error('Continuing only under recorded product decision ' + decision.decision + ' (this is not a passing clear).');
  }
  return { verdict, observed, product_decision: decision.decision };
}

function readProductDecision(verdict, observed) {
  let doc;
  try {
    doc = JSON.parse(fs.readFileSync(DECISION_PATH, 'utf8'));
  } catch (err) {
    return { ok: false, reason: 'cannot read product decision ' + DECISION_PATH + ': ' + err.message };
  }
  if (doc.probe_verdict !== verdict) {
    return { ok: false, reason: `recorded probe_verdict ${doc.probe_verdict} differs from the observed ${verdict}` };
  }
  if (verdict === FAILED_VERDICT && (doc.decision !== RELAX_DECISION || doc.client_cleared_buffered_playback !== false)) {
    return { ok: false, reason: `an unresolved clear needs decision ${RELAX_DECISION} with client_cleared_buffered_playback=false` };
  }
  const recorded = doc.probes || {};
  if (Object.keys(recorded).length !== Object.keys(observed).length) {
    return { ok: false, reason: 'recorded probes name other signals than this probe sends' };
  }
  for (const [name, got] of Object.entries(observed)) {
    if (JSON.stringify(recorded[name]) !== JSON.stringify(got)) {
      return { ok: false, reason: `recorded probe ${name} ${JSON.stringify(recorded[name])} differs from the observed ${JSON.stringify(got)}` };
    }
  }
  return { ok: true, decision: doc.decision };
}

if (require.main === module) {
  runPlaybackClearProbe().catch((err) => {
    console.error('Probe failed unexpectedly:', err);
    process.exit(1);
  });
}

module.exports = { runPlaybackClearProbe };
