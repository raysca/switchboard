const fs = require('fs');
const path = require('path');
const { ProtobufFrameSerializer } = require('@pipecat-ai/websocket-transport');

async function verifyFixtures() {
  console.log('=== Verifying Protobuf Fixtures with JS Serializer ===');
  const fixturesDir = path.join(__dirname, 'fixtures');
  const serializer = new ProtobufFrameSerializer();

  // 1. Verify client-audio.bin
  const clientAudioBytes = fs.readFileSync(path.join(fixturesDir, 'client-audio.bin'));
  const clientAudio = await serializer.deserialize(new Blob([clientAudioBytes]));
  if (clientAudio.type !== 'audio' || clientAudio.audio.length !== 320) {
    throw new Error(`client-audio.bin verification failed: type=${clientAudio.type}, len=${clientAudio.audio?.length}`);
  }
  console.log(' client-audio.bin valid: 320 PCM16 samples (20ms @ 16kHz)');

  // 2. Verify server-audio.bin
  const serverAudioBytes = fs.readFileSync(path.join(fixturesDir, 'server-audio.bin'));
  const serverAudio = await serializer.deserialize(new Blob([serverAudioBytes]));
  if (serverAudio.type !== 'audio' || serverAudio.audio.length !== 480) {
    throw new Error(`server-audio.bin verification failed: type=${serverAudio.type}, len=${serverAudio.audio?.length}`);
  }
  console.log(' server-audio.bin valid: 480 PCM16 samples (20ms @ 24kHz)');

  // 3. Verify nested-rtvi.bin
  const nestedRtviBytes = fs.readFileSync(path.join(fixturesDir, 'nested-rtvi.bin'));
  const nestedRtvi = await serializer.deserialize(new Blob([nestedRtviBytes]));
  if (nestedRtvi.type !== 'message' || nestedRtvi.message.type !== 'bot-ready') {
    throw new Error(`nested-rtvi.bin verification failed: type=${nestedRtvi.type}, msgType=${nestedRtvi.message?.type}`);
  }
  console.log(' nested-rtvi.bin valid: label="rtvi-ai", type="bot-ready"');

  // 4. Verify malformed.bin fails explicitly
  const malformedBytes = fs.readFileSync(path.join(fixturesDir, 'malformed.bin'));
  let malformedFailed = false;
  try {
    await serializer.deserialize(new Blob([malformedBytes]));
  } catch (err) {
    malformedFailed = true;
    console.log(' malformed.bin failed explicitly as expected:', err.message);
  }
  if (!malformedFailed) {
    throw new Error('malformed.bin should have thrown an error but succeeded');
  }

  // 5. Verify wrong-schema.bin fails explicitly
  const wrongSchemaBytes = fs.readFileSync(path.join(fixturesDir, 'wrong-schema.bin'));
  let wrongSchemaFailed = false;
  try {
    await serializer.deserialize(new Blob([wrongSchemaBytes]));
  } catch (err) {
    wrongSchemaFailed = true;
    console.log(' wrong-schema.bin failed explicitly as expected:', err.message);
  }
  if (!wrongSchemaFailed) {
    throw new Error('wrong-schema.bin should have thrown an error but succeeded');
  }

  console.log('\n All JS fixture verifications PASSED.');
}

if (require.main === module) {
  verifyFixtures().catch(err => {
    console.error('Fixture verification failed:', err);
    process.exit(1);
  });
}

module.exports = { verifyFixtures };
