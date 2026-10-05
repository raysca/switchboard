/**
 * test_rtvi_client.js
 *
 * Client regression gate verifying that the unmodified, released
 * @pipecat-ai/client-js@1.13.1 and @pipecat-ai/websocket-transport@1.7.2 packages
 * connect to the Go Voice Framework server, negotiate RTVI readiness,
 * send/receive audio, receive RTVI events, and tear down cleanly.
 */

// Browser stubs for Node runtime
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

async function runClientTest(wsUrl, token) {
  console.log(`[RTVI Client Test] Connecting to ${wsUrl}...`);

  let botReadyReceived = false;
  let botReadyVersion = null;
  let userTranscriptReceived = false;
  const transcriptEvents = [];
  const eventOrder = [];
  let botLlmTextReceived = false;
  let botStartedSpeakingReceived = false;
  let botStoppedSpeakingReceived = false;
  let userStartedSpeakingReceived = false;
  let userStoppedSpeakingReceived = false;
  let botAudioBuffersReceived = 0;
  let userAudioCallback = null;
  let fatalError = null;
  let serverErrorReceived = false;

  const mockMediaManager = {
    initialize: async () => {},
    connect: async () => {},
    disconnect: async () => {},
    setUserAudioCallback: (cb) => {
      userAudioCallback = cb;
    },
    setClientOptions: () => {},
    tracks: () => ({}),
    userStartedSpeaking: async () => { userStartedSpeakingReceived = true; },
    userStoppedSpeaking: async () => { userStoppedSpeakingReceived = true; },
    bufferBotAudio: () => { botAudioBuffersReceived++; }
  };

  const transport = new WebSocketTransport({ mediaManager: mockMediaManager });
  const client = new PipecatClient({
    transport: transport,
    enableMic: false,
    enableCam: false,
    callbacks: {
      onBotReady: (data) => {
        botReadyReceived = true;
        botReadyVersion = data?.version;
        console.log(`[RTVI Client Test] onBotReady received: version=${botReadyVersion}`);
      },
      onUserTranscript: (data) => {
        userTranscriptReceived = true;
        transcriptEvents.push({text: data?.text, final: data?.final});
        eventOrder.push(data?.final ? 'transcript-final' : 'transcript-interim');
        console.log(`[RTVI Client Test] onUserTranscript received: "${data?.text}"`);
      },
      onUserStartedSpeaking: () => { userStartedSpeakingReceived = true; },
      onUserStoppedSpeaking: () => { userStoppedSpeakingReceived = true; },
      onBotLlmText: (data) => {
        botLlmTextReceived = true;
        console.log(`[RTVI Client Test] onBotLlmText received: "${data?.text}"`);
      },
      onBotStartedSpeaking: () => {
        botStartedSpeakingReceived = true;
        console.log(`[RTVI Client Test] onBotStartedSpeaking received`);
      },
      onBotStoppedSpeaking: () => {
        botStoppedSpeakingReceived = true;
        console.log(`[RTVI Client Test] onBotStoppedSpeaking received`);
      },
      onError: (err) => {
        console.error(`[RTVI Client Test] onError received:`, err);
        if (err?.data?.error === 'compat_test_error' && err?.data?.fatal === false) {
          serverErrorReceived = true;
          eventOrder.push('server-error');
        } else {
          fatalError = err;
        }
      }
    }
  });

  // Step 1: Connect and complete client-ready -> bot-ready handshake with bounded timeout
  const connectPromise = client.connect({
    wsUrl: wsUrl,
    token: token
  });
  const timeoutPromise = new Promise((_, reject) => {
    const timer = setTimeout(() => {
      if (fatalError) {
        reject(new Error(`Connection failed with fatal error: ${JSON.stringify(fatalError)}`));
      } else {
        reject(new Error("client.connect timed out after 5000ms"));
      }
    }, 5000);
    // Unref timer so node process can exit if needed
    if (timer.unref) timer.unref();
  });

  await Promise.race([connectPromise, timeoutPromise]);

  if (!botReadyReceived || botReadyVersion !== "2.1.0") {
    throw new Error(`Bot ready failed: received=${botReadyReceived}, version=${botReadyVersion}`);
  }
  console.log('[RTVI Client Test] Handshake verified with version 2.1.0.');

  // Step 2: Simulate client microphone audio sending
  if (userAudioCallback) {
    console.log('[RTVI Client Test] Sending 16kHz PCM audio chunk...');
    const samples = new Int16Array(320); // 20ms @ 16kHz
    userAudioCallback(samples.buffer);
  }

  // Wait for round trip events
  const startTime = Date.now();
  while (Date.now() - startTime < 3000) {
    if (transcriptEvents.length >= 2 && serverErrorReceived && userStartedSpeakingReceived && userStoppedSpeakingReceived && botLlmTextReceived && botStartedSpeakingReceived && botStoppedSpeakingReceived && botAudioBuffersReceived > 0) {
      break;
    }
    await new Promise(r => setTimeout(r, 50));
  }

  console.log(`[RTVI Client Test] Events received:`, {
    botReady: botReadyReceived,
    userTranscript: userTranscriptReceived,
    userStartedSpeaking: userStartedSpeakingReceived,
    userStoppedSpeaking: userStoppedSpeakingReceived,
    botLlmText: botLlmTextReceived,
    botStartedSpeaking: botStartedSpeakingReceived,
    botStoppedSpeaking: botStoppedSpeakingReceived,
    botAudioBuffers: botAudioBuffersReceived
  });
  if (!userTranscriptReceived || !userStartedSpeakingReceived || !userStoppedSpeakingReceived || !botLlmTextReceived || !botStartedSpeakingReceived || !botStoppedSpeakingReceived || botAudioBuffersReceived === 0) {
    throw new Error(`Incomplete client conformance: ${JSON.stringify({userTranscriptReceived, userStartedSpeakingReceived, userStoppedSpeakingReceived, botLlmTextReceived, botStartedSpeakingReceived, botStoppedSpeakingReceived, botAudioBuffersReceived})}`);
  }
  if (transcriptEvents.length !== 2 || transcriptEvents[0].final !== false || transcriptEvents[0].text !== 'Hello from' || transcriptEvents[1].final !== true || transcriptEvents[1].text !== 'Hello from unmodified client-js') {
    throw new Error(`Interim/final transcript payload/order mismatch: ${JSON.stringify(transcriptEvents)}`);
  }
  if (!serverErrorReceived || eventOrder.indexOf('server-error') <= eventOrder.indexOf('transcript-final')) {
    throw new Error(`Nonfatal server error missing or out of order: ${JSON.stringify(eventOrder)}`);
  }

  // Step 3: Disconnect cleanly
  console.log('[RTVI Client Test] Disconnecting...');
  await client.disconnect();
  console.log('[RTVI Client Test] Disconnected successfully.');

  return {
    success: true,
    botReady: botReadyReceived,
    botVersion: botReadyVersion,
    userTranscript: userTranscriptReceived,
    botLlmText: botLlmTextReceived,
    botStartedSpeaking: botStartedSpeakingReceived,
    botStoppedSpeaking: botStoppedSpeakingReceived
  };
}

if (require.main === module) {
  const args = process.argv.slice(2);
  const urlIndex = args.indexOf('--url');
  const tokenIndex = args.indexOf('--token');

  if (urlIndex === -1 || tokenIndex === -1) {
    console.error('Usage: node test_rtvi_client.js --url <wsUrl> --token <token>');
    process.exit(1);
  }

  const wsUrl = args[urlIndex + 1];
  const token = args[tokenIndex + 1];

  runClientTest(wsUrl, token)
    .then(res => {
      console.log('RTVI Client Test PASSED:', JSON.stringify(res));
      process.exit(0);
    })
    .catch(err => {
      console.error('RTVI Client Test FAILED:', err);
      process.exit(1);
    });
}

module.exports = { runClientTest };
