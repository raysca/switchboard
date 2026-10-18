const fs = require('fs');
const path = require('path');
const crypto = require('crypto');

function sha256File(filePath) {
  const fileBuffer = fs.readFileSync(filePath);
  const hashSum = crypto.createHash('sha256');
  hashSum.update(fileBuffer);
  return hashSum.digest('hex');
}

function validateManifest() {
  const rootDir = path.resolve(__dirname, '../..');
  const manifestPath = path.join(rootDir, 'docs/compatibility/v1-lock.json');

  if (!fs.existsSync(manifestPath)) {
    throw new Error(`Manifest not found at ${manifestPath}`);
  }

  const manifest = JSON.parse(fs.readFileSync(manifestPath, 'utf8'));

  // 1. Check required top-level keys
  const requiredKeys = [
    'name', 'version', 'toolchain', 'modules', 'client_packages',
    'protobuf', 'rtvi', 'audio', 'resampler', 'vad', 'smart_turn',
    'google_provider', 'xai_provider', 'auth_and_admission',
    'limits_and_timeouts', 'target_platform', 'slos'
  ];

  for (const key of requiredKeys) {
    if (!manifest[key]) {
      throw new Error(`Missing required manifest key: ${key}`);
    }
  }

  // 2. Reject floating aliases ("latest", empty, wildcard, placeholders)
  function checkNoFloating(obj, pathStr = '') {
    for (const [k, v] of Object.entries(obj)) {
      const curPath = pathStr ? `${pathStr}.${k}` : k;
      if (typeof v === 'string') {
        if (v === 'latest' || v.endsWith('-latest') || v === '*' || v.includes('^') || v.includes('~')) {
          throw new Error(`Floating version alias detected at ${curPath}: "${v}"`);
        }
        if (v.includes('<') || v.includes('>')) {
          throw new Error(`Unresolved placeholder detected at ${curPath}: "${v}"`);
        }
        if (v.trim() === '') {
          throw new Error(`Empty value detected at ${curPath}`);
        }
      } else if (typeof v === 'object' && v !== null && !Array.isArray(v)) {
        checkNoFloating(v, curPath);
      }
    }
  }
  checkNoFloating(manifest);

  // 3. Verify proto file checksum
  const protoPath = path.join(rootDir, manifest.protobuf.source_file);
  if (!fs.existsSync(protoPath)) {
    throw new Error(`Proto source file not found: ${protoPath}`);
  }
  const actualProtoSha = sha256File(protoPath);
  if (actualProtoSha !== manifest.protobuf.sha256) {
    throw new Error(`Proto SHA-256 mismatch! Expected ${manifest.protobuf.sha256}, got ${actualProtoSha}`);
  }

  // 4. Verify Silero VAD checksum
  const vadPath = path.join(rootDir, manifest.vad.model_file);
  if (!fs.existsSync(vadPath)) {
    throw new Error(`VAD model file not found: ${vadPath}`);
  }
  const actualVadSha = sha256File(vadPath);
  if (actualVadSha !== manifest.vad.sha256) {
    throw new Error(`VAD SHA-256 mismatch! Expected ${manifest.vad.sha256}, got ${actualVadSha}`);
  }

  // 4b. Verify Smart Turn model checksum
  const turnPath = path.join(rootDir, manifest.smart_turn.model_file);
  if (!fs.existsSync(turnPath)) {
    throw new Error(`Smart Turn model file not found: ${turnPath}`);
  }
  const actualTurnSha = sha256File(turnPath);
  if (actualTurnSha !== manifest.smart_turn.sha256) {
    throw new Error(`Smart Turn SHA-256 mismatch! Expected ${manifest.smart_turn.sha256}, got ${actualTurnSha}`);
  }
  // The pin is repeated in the Go loader and the image build; keep all three in step.
  for (const rel of ['smartturn/model.go', 'deploy/Dockerfile']) {
    if (!fs.readFileSync(path.join(rootDir, rel), 'utf8').includes(manifest.smart_turn.sha256)) {
      throw new Error(`Smart Turn SHA-256 in ${rel} does not match the manifest`);
    }
  }

  // The output queue bound is recorded in the lock and in the playback-clear decision; they must agree.
  const decision = JSON.parse(fs.readFileSync(path.join(rootDir, 'docs/compatibility/playback-clear-decision.json'), 'utf8'));
  const residual = decision.v1_server_side_residual_playback;
  const limits = manifest.limits_and_timeouts.default_websocket;
  for (const [lockKey, decisionKey] of [['output_queue_duration_ms', 'maximum_queued_duration_ms'], ['output_queue_bytes', 'maximum_queued_bytes']]) {
    if (limits[lockKey] !== residual[decisionKey]) {
      throw new Error(`${lockKey} (${limits[lockKey]}) differs from playback-clear-decision ${decisionKey} (${residual[decisionKey]})`);
    }
  }

  // 5. Verify installed npm package versions in tools/compat
  for (const [pkgName, pkgMeta] of Object.entries(manifest.client_packages)) {
    const pkgJsonPath = path.join(__dirname, 'node_modules', pkgName, 'package.json');
    if (!fs.existsSync(pkgJsonPath)) {
      throw new Error(`Installed npm package ${pkgName} not found at ${pkgJsonPath}`);
    }
    const installedPkg = JSON.parse(fs.readFileSync(pkgJsonPath, 'utf8'));
    if (installedPkg.version !== pkgMeta.version) {
      throw new Error(`NPM version mismatch for ${pkgName}: manifest expects ${pkgMeta.version}, found ${installedPkg.version}`);
    }
  }

  console.log(' Manifest validation PASSED: all immutable coordinates, checksums, and package versions verified.');
}

if (require.main === module) {
  try {
    validateManifest();
  } catch (err) {
    console.error(' Manifest validation FAILED:', err.message);
    process.exit(1);
  }
}

module.exports = { validateManifest };
