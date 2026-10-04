type CapabilityEnvironment = {
  isSecureContext?: boolean;
  navigator?: { mediaDevices?: { getUserMedia?: unknown } };
  MediaRecorder?: unknown;
};

export function getBrowserCapabilities(
  environment: CapabilityEnvironment = globalThis,
) {
  return {
    secureContext: environment.isSecureContext === true,
    microphone:
      typeof environment.navigator?.mediaDevices?.getUserMedia === "function",
    mediaRecorder: typeof environment.MediaRecorder === "function",
  };
}
