const fs = require('fs');
const path = require('path');
const crypto = require('crypto');
const { ProtobufFrameSerializer } = require('@pipecat-ai/websocket-transport');

function sha256Buffer(buf) {
  return crypto.createHash('sha256').update(buf).digest('hex');
}

function generateFixtures() {
  const fixturesDir = path.join(__dirname, 'fixtures');
  if (!fs.existsSync(fixturesDir)) {
    fs.mkdirSync(fixturesDir, { recursive: true });
  }

  const serializer = new ProtobufFrameSerializer();

  // 1. Client Audio Fixture: 20ms of 16000 Hz PCM16LE mono (320 samples, 640 bytes)
  const clientSampleCount = 320;
  const clientSamples = new Int16Array(clientSampleCount);
  for (let i = 0; i < clientSampleCount; i++) {
    // Generate deterministic 440 Hz tone at 16000 Hz sample rate
    clientSamples[i] = Math.round(10000 * Math.sin((2 * Math.PI * 440 * i) / 16000));
  }
  const clientAudioBytes = serializer.serializeAudio(clientSamples.buffer, 16000, 1);
  const clientAudioPath = path.join(fixturesDir, 'client-audio.bin');
  fs.writeFileSync(clientAudioPath, Buffer.from(clientAudioBytes));

  // 2. Server Audio Fixture: 20ms of 24000 Hz PCM16LE mono (480 samples, 960 bytes)
  const serverSampleCount = 480;
  const serverSamples = new Int16Array(serverSampleCount);
  for (let i = 0; i < serverSampleCount; i++) {
    // Generate deterministic 440 Hz tone at 24000 Hz sample rate
    serverSamples[i] = Math.round(10000 * Math.sin((2 * Math.PI * 440 * i) / 24000));
  }
  const serverAudioBytes = serializer.serializeAudio(serverSamples.buffer, 24000, 1);
  const serverAudioPath = path.join(fixturesDir, 'server-audio.bin');
  fs.writeFileSync(serverAudioPath, Buffer.from(serverAudioBytes));

  // 3. Nested RTVI Message Fixture
  const rtviMsg = {
    id: "compat-fixture-001",
    label: "rtvi-ai",
    type: "bot-ready",
    data: {
      version: "2.1.0",
      features: {
        audio: true
      }
    }
  };
  const nestedRtviBytes = serializer.serializeMessage(rtviMsg);
  const nestedRtviPath = path.join(fixturesDir, 'nested-rtvi.bin');
  fs.writeFileSync(nestedRtviPath, Buffer.from(nestedRtviBytes));

  // 4. Malformed Protobuf Fixture: corrupted wire stream
  const malformedBytes = Buffer.from([0xff, 0xff, 0xff, 0xff, 0x7f, 0x01, 0x02]);
  const malformedPath = path.join(fixturesDir, 'malformed.bin');
  fs.writeFileSync(malformedPath, malformedBytes);

  // 5. Wrong Schema Fixture: field 5 (interruption) or unrecognized structure
  // Field 99 (varint), which is not part of pipecat.Frame
  const wrongSchemaBytes = Buffer.from([0x98, 0x06, 0x2a]);
  const wrongSchemaPath = path.join(fixturesDir, 'wrong-schema.bin');
  fs.writeFileSync(wrongSchemaPath, wrongSchemaBytes);

  // Manifest of generated fixtures with exact package, schema, and protocol coordinates
  const coordinates = {
    packages: {
      "@pipecat-ai/websocket-transport": "1.7.2",
      "@pipecat-ai/client-js": "1.13.1"
    },
    schema: {
      source_file: "proto/frames.proto",
      pipecat_reference_commit: "2967e1c09ad484076089bc88427d305fe8f79fa9",
      sha256: "adcd346af21673d3385d075c5a662a8407584b4b04f177320609596f4dea5c37"
    },
    protocol: {
      rtvi_version: "2.1.0",
      interruption_event: "user-started-speaking",
      clear_event: "bot-stopped-speaking"
    }
  };

  const provenance = {
    generator: "tools/compat/generate_fixtures.js",
    generated_at: "2026-09-27T10:00:00Z",
    coordinates: coordinates,
    packages: coordinates.packages,
    fixtures: {
      "client-audio.bin": {
        size: clientAudioBytes.length,
        sha256: sha256Buffer(Buffer.from(clientAudioBytes)),
        description: "20ms 16kHz PCM16LE mono client audio in Protobuf AudioRawFrame",
        coordinates: coordinates
      },
      "server-audio.bin": {
        size: serverAudioBytes.length,
        sha256: sha256Buffer(Buffer.from(serverAudioBytes)),
        description: "20ms 24kHz PCM16LE mono server audio in Protobuf AudioRawFrame",
        coordinates: coordinates
      },
      "nested-rtvi.bin": {
        size: nestedRtviBytes.length,
        sha256: sha256Buffer(Buffer.from(nestedRtviBytes)),
        description: "Protobuf MessageFrame carrying bot-ready RTVI JSON payload",
        coordinates: coordinates
      },
      "malformed.bin": {
        size: malformedBytes.length,
        sha256: sha256Buffer(malformedBytes),
        description: "Malformed protobuf wire bytes for failure testing",
        coordinates: coordinates
      },
      "wrong-schema.bin": {
        size: wrongSchemaBytes.length,
        sha256: sha256Buffer(wrongSchemaBytes),
        description: "Protobuf bytes with unknown field tags outside Frame definition",
        coordinates: coordinates
      }
    }
  };

  fs.writeFileSync(path.join(fixturesDir, 'provenance.json'), JSON.stringify(provenance, null, 2));
  console.log(' Generated all fixtures successfully in', fixturesDir);
  console.log(JSON.stringify(provenance.fixtures, null, 2));
}

if (require.main === module) {
  generateFixtures();
}

module.exports = { generateFixtures };
