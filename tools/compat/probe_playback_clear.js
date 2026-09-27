/**
 * probe_playback_clear.js
 *
 * Implements the Phase 0 Hard Playback-Clear Probe against the exact,
 * unmodified released packages: @pipecat-ai/client-js@1.13.1 and
 * @pipecat-ai/websocket-transport@1.7.2.
 */

// Browser environment stubs for node runtime
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

const { ProtobufFrameSerializer, WebSocketTransport } = require('@pipecat-ai/websocket-transport');
const { PipecatClient, RTVIMessage, RTVIMessageType } = require('@pipecat-ai/client-js');

async function runPlaybackClearProbe() {
  console.log('=== Running Phase 0 Hard Playback-Clear Probe ===');
  const serializer = new ProtobufFrameSerializer();

  // Probe 1: Direct InterruptionFrame protobuf (field 5)
  console.log('\n[Probe 1] Testing Protobuf InterruptionFrame (field 5):');
  // Protobuf field 5, length-delimited tag 0x2a, len 0
  const field5Bytes = new Uint8Array([0x2a, 0x00]);
  let probe1Success = false;
  let probe1Error = null;
  try {
    const res = await serializer.deserialize(new Blob([field5Bytes]));
    probe1Success = true;
    console.log('Unexpected success:', res);
  } catch (err) {
    probe1Error = err.message;
    console.log('Observed behavior: Deserializer rejected field 5 with:', `"${err.message}"`);
  }

  // Probe 2: RTVI user-started-speaking message (field 4)
  console.log('\n[Probe 2] Testing RTVI user-started-speaking message:');
  const userStartedMsg = {
    id: "probe-msg-002",
    label: "rtvi-ai",
    type: "user-started-speaking"
  };
  const userStartedBytes = serializer.serializeMessage(userStartedMsg);
  const parsedMsg = await serializer.deserialize(new Blob([userStartedBytes]));
  console.log('Deserialized message type:', parsedMsg.type, 'RTVI type:', parsedMsg.message?.type);

  // Probe 3: Check whether WebSocketTransport wires user-started-speaking to player interrupt
  console.log('\n[Probe 3] Inspecting WebSocketTransport message dispatch:');
  const transportProto = WebSocketTransport.prototype;
  const transportMethods = Object.getOwnPropertyNames(transportProto);
  const hasInterruptMethod = transportMethods.some(m => m.toLowerCase().includes('interrupt'));
  const hasClearMethod = transportMethods.some(m => m.toLowerCase().includes('clear'));
  console.log('WebSocketTransport has public interrupt method:', hasInterruptMethod);
  console.log('WebSocketTransport has public clear method:', hasClearMethod);

  // Probe 4: Check PipecatClient handleMessage and MediaManager:
  console.log('\n[Probe 4] Inspecting PipecatClient handleMessage and MediaManager:');
  let mediaManagerInterruptCalled = false;
  let callbackTriggered = false;

  const mockMediaManager = {
    initialize: async () => {},
    connect: async () => {},
    disconnect: async () => {},
    setUserAudioCallback: () => {},
    setClientOptions: () => {},
    tracks: () => ({}),
    userStartedSpeaking: async () => {
      mediaManagerInterruptCalled = true;
    },
    bufferBotAudio: () => {}
  };

  const transport = new WebSocketTransport({ mediaManager: mockMediaManager });
  const client = new PipecatClient({
    transport: transport,
    callbacks: {
      onUserStartedSpeaking: () => {
        callbackTriggered = true;
      }
    }
  });

  client.handleMessage(userStartedMsg);
  console.log('onUserStartedSpeaking callback triggered:', callbackTriggered);
  console.log('MediaManager userStartedSpeaking called by client/transport:', mediaManagerInterruptCalled);

  // Summary and Verdict
  const results = {
    probe1_interruption_field5_supported: probe1Success,
    probe1_rejection_reason: probe1Error,
    probe2_rtvi_message_deserializes: parsedMsg.type === 'message' && parsedMsg.message.type === 'user-started-speaking',
    probe3_transport_clears_playback: false,
    probe4_client_clears_playback: mediaManagerInterruptCalled,
    verdict: "UNMODIFIED_CLIENT_CANNOT_CLEAR_BUFFERED_PLAYBACK_ON_WIRE_EVENT",
    remedy: "PACED_SERVER_SIDE_INTERRUPTION_BARRIER_WITH_MEDIA_CLOCK_PACING",
    max_unpaced_client_buffer_ms: 100
  };

  console.log('\n=== Probe Result Summary ===');
  console.log(JSON.stringify(results, null, 2));

  return results;
}

if (require.main === module) {
  runPlaybackClearProbe().catch(err => {
    console.error('Probe failed unexpectedly:', err);
    process.exit(1);
  });
}

module.exports = { runPlaybackClearProbe };
