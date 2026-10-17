/**
 * test_rtvi_barge_in.js
 *
 * The released @pipecat-ai/client-js@1.13.1 with @pipecat-ai/websocket-transport@1.7.2 against a running
 * Switchboard server: a long reply is cut off by a typed turn while its audio plays. Prints, as one JSON line
 * after "EVENTS ", what the client handled in order ("msg:<type>" for every RTVI message, read from the
 * client's own debug line "[RTVI Message]", and "audio" for every chunk handed to the media manager), how
 * often it asked the media manager to interrupt playback, and every error it logged before disconnecting.
 * Driven by server/barge_in_client_e2e_test.go.
 *
 * Usage: node test_rtvi_barge_in.js --url <wsUrl> --token <token> --long <text> --barge <text>
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

async function until(cond, ms, what) {
  const end = Date.now() + ms;
  while (!cond()) {
    if (Date.now() > end) throw new Error(`timed out waiting for ${what}`);
    await new Promise((r) => setTimeout(r, 20));
  }
}

async function main() {
  const log = [];
  const errors = [];
  const said = [];
  let interrupts = 0;
  const debug = console.debug;
  const error = console.error;
  // The client logs every RTVI message it handles as console.debug('[RTVI Message]', msg) (its logger is at
  // DEBUG by default), types it does not recognise included; WebSocketTransport logs a frame it cannot
  // deserialize with console.error. Both are observed here, the packages are not changed.
  console.debug = (...a) => {
    if (a[0] === '[RTVI Message]' && a[1] && a[1].type) log.push('msg:' + a[1].type);
  };
  console.error = (...a) => errors.push(a.map(String).join(' '));
  const mediaManager = {
    initialize: async () => {}, connect: async () => {}, disconnect: async () => {},
    setUserAudioCallback: () => {}, setClientOptions: () => {}, tracks: () => ({}),
    userStartedSpeaking: async () => { interrupts++; }, bufferBotAudio: () => { log.push('audio'); },
  };
  const client = new PipecatClient({
    transport: new WebSocketTransport({ mediaManager }),
    enableMic: false,
    enableCam: false,
    callbacks: {
      onBotLlmText: (d) => said.push((d && d.text) || ''),
      onError: (e) => errors.push('onError ' + JSON.stringify(e)),
    },
  });
  let seen;
  try {
    await Promise.race([
      client.connect({ wsUrl: arg('--url'), token: arg('--token') }),
      new Promise((_, reject) => setTimeout(() => reject(new Error('connect timed out')), 5000)),
    ]);
    await client.sendText(arg('--long'));
    await until(() => log.filter((e) => e === 'audio').length >= 10, 20000, 'the long reply to play');
    await client.sendText(arg('--barge'));
    // The barge-in's flush stops the speaking state once, the barge-in reply's end stops it again.
    await until(() => said.join('').includes(arg('--barge')) && log.includes('msg:bot-interrupted')
      && log.filter((e) => e === 'msg:bot-stopped-speaking').length >= 2, 20000, 'the barge-in reply to finish');
    seen = errors.slice();
    await client.disconnect();
  } finally {
    console.debug = debug;
    console.error = error;
  }
  console.log('EVENTS ' + JSON.stringify({ log, interrupts, errors: seen }));
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
