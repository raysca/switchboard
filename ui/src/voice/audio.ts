/**
 * Browser Audio Capture and Playback Manager for RTVI voice sessions.
 */

export type AudioErrorType =
  | "permission_denied"
  | "device_not_found"
  | "unsupported"
  | "audio_context_error"
  | "cancelled"; // stopCapture()/destroy() ran while the mic was opening

export class BrowserAudioError extends Error {
  constructor(
    message: string,
    public readonly type: AudioErrorType,
    public readonly originalError?: unknown,
  ) {
    super(message);
    this.name = "BrowserAudioError";
  }
}

export class BrowserAudioController {
  private mediaStream: MediaStream | null = null;
  private inputContext: AudioContext | null = null;
  private outputContext: AudioContext | null = null;
  private scriptProcessor: ScriptProcessorNode | null = null;
  private mediaSource: MediaStreamAudioSourceNode | null = null;

  private isMuted = false;
  private activeSources: AudioBufferSourceNode[] = [];
  private nextPlaybackTime = 0;
  // Bumped by stopCapture(); a startCapture() that sees it change mid-getUserMedia gives the mic back.
  private captureGen = 0;

  /**
   * Requests microphone access and begins streaming 16kHz PCM16LE mono audio chunks.
   */
  public async startCapture(onAudioChunk: (chunk: Uint8Array) => void): Promise<void> {
    if (typeof navigator === "undefined" || !navigator.mediaDevices?.getUserMedia) {
      throw new BrowserAudioError(
        "Microphone input is not supported by this browser environment.",
        "unsupported",
      );
    }

    const gen = this.captureGen;
    let stream: MediaStream;
    try {
      stream = await navigator.mediaDevices.getUserMedia({
        audio: {
          echoCancellation: true,
          noiseSuppression: true,
          autoGainControl: true,
          channelCount: 1,
          sampleRate: 16000,
        },
      });
    } catch (err: unknown) {
      const errorName = (err as { name?: string })?.name ?? "";
      if (errorName === "NotAllowedError" || errorName === "PermissionDeniedError") {
        throw new BrowserAudioError(
          "Microphone permission was denied. Please allow microphone access in your browser settings.",
          "permission_denied",
          err,
        );
      }
      if (errorName === "NotFoundError" || errorName === "DevicesNotFoundError") {
        throw new BrowserAudioError(
          "No microphone device was detected on your system.",
          "device_not_found",
          err,
        );
      }
      throw new BrowserAudioError(
        `Failed to access microphone: ${(err as Error)?.message ?? String(err)}`,
        "audio_context_error",
        err,
      );
    }
    if (gen !== this.captureGen) {
      for (const track of stream.getTracks()) track.stop();
      throw new BrowserAudioError("Microphone request was cancelled.", "cancelled");
    }
    this.mediaStream = stream;

    try {
      const AudioCtx =
        window.AudioContext ||
        (window as unknown as { webkitAudioContext: typeof AudioContext }).webkitAudioContext;
      this.inputContext = new AudioCtx({ sampleRate: 16000 });
      if (this.inputContext.state === "suspended") {
        await this.inputContext.resume();
      }

      this.mediaSource = this.inputContext.createMediaStreamSource(stream);
      // ScriptProcessorNode: 512 samples per chunk (32ms @ 16kHz) matches 10-40ms chunk requirement
      this.scriptProcessor = this.inputContext.createScriptProcessor(512, 1, 1);

      this.scriptProcessor.onaudioprocess = (event) => {
        if (this.isMuted) return;

        const inputBuffer = event.inputBuffer.getChannelData(0);
        const pcm16 = this.convertFloat32ToPCM16(inputBuffer);
        onAudioChunk(new Uint8Array(pcm16.buffer));
      };

      this.mediaSource.connect(this.scriptProcessor);
      this.scriptProcessor.connect(this.inputContext.destination);
    } catch (err) {
      this.stopCapture();
      throw new BrowserAudioError(
        `Failed to initialize audio capture pipeline: ${(err as Error)?.message ?? String(err)}`,
        "audio_context_error",
        err,
      );
    }
  }

  /**
   * Stops microphone capture and releases hardware devices.
   */
  public stopCapture(): void {
    this.captureGen++;
    if (this.mediaStream) {
      for (const track of this.mediaStream.getTracks()) {
        try {
          track.stop();
        } catch {
          // Ignore
        }
      }
      this.mediaStream = null;
    }

    if (this.scriptProcessor) {
      try {
        this.scriptProcessor.disconnect();
      } catch {
        // Ignore
      }
      this.scriptProcessor = null;
    }

    if (this.mediaSource) {
      try {
        this.mediaSource.disconnect();
      } catch {
        // Ignore
      }
      this.mediaSource = null;
    }

    if (this.inputContext) {
      try {
        void this.inputContext.close();
      } catch {
        // Ignore
      }
      this.inputContext = null;
    }
  }

  /**
   * Sets microphone mute state.
   */
  public setMuted(muted: boolean): void {
    this.isMuted = muted;
    if (this.mediaStream) {
      for (const track of this.mediaStream.getAudioTracks()) {
        track.enabled = !muted;
      }
    }
  }

  /**
   * Initializes or returns the output AudioContext.
   */
  private getOutputContext(sampleRate = 24000): AudioContext {
    if (!this.outputContext || this.outputContext.state === "closed") {
      const AudioCtx =
        window.AudioContext ||
        (window as unknown as { webkitAudioContext: typeof AudioContext }).webkitAudioContext;
      this.outputContext = new AudioCtx({ sampleRate });
      this.nextPlaybackTime = 0;
    }
    if (this.outputContext.state === "suspended") {
      void this.outputContext.resume();
    }
    return this.outputContext;
  }

  /**
   * Plays an incoming PCM16LE mono audio chunk (typically 24kHz from Go server).
   */
  public playAudioChunk(pcm16Bytes: Uint8Array, sampleRate = 24000): void {
    if (pcm16Bytes.length === 0) return;

    try {
      const ctx = this.getOutputContext(sampleRate);
      const float32 = this.convertPCM16ToFloat32(pcm16Bytes);
      const audioBuffer = ctx.createBuffer(1, float32.length, sampleRate);
      audioBuffer.copyToChannel(float32 as unknown as Float32Array<ArrayBuffer>, 0);

      const source = ctx.createBufferSource();
      source.buffer = audioBuffer;
      source.connect(ctx.destination);

      const currentTime = ctx.currentTime;
      if (this.nextPlaybackTime < currentTime) {
        this.nextPlaybackTime = currentTime;
      }

      source.start(this.nextPlaybackTime);
      this.nextPlaybackTime += audioBuffer.duration;

      this.activeSources.push(source);
      source.onended = () => {
        const idx = this.activeSources.indexOf(source);
        if (idx !== -1) {
          this.activeSources.splice(idx, 1);
        }
      };
    } catch {
      // In headless test environments or audio error, ignore gracefully
    }
  }

  /**
   * Immediately stops any currently playing audio and flushes buffered chunks.
   * Essential for barge-in UX when user interrupts the bot.
   */
  public stopPlayback(): void {
    for (const source of this.activeSources) {
      try {
        source.stop();
        source.disconnect();
      } catch {
        // Ignore
      }
    }
    this.activeSources = [];
    if (this.outputContext) {
      this.nextPlaybackTime = this.outputContext.currentTime;
    } else {
      this.nextPlaybackTime = 0;
    }
  }

  /**
   * Full teardown of both capture and playback systems.
   */
  public destroy(): void {
    this.stopCapture();
    this.stopPlayback();
    if (this.outputContext) {
      try {
        void this.outputContext.close();
      } catch {
        // Ignore
      }
      this.outputContext = null;
    }
  }

  // --- Audio Format Conversion Utilities ---

  private convertFloat32ToPCM16(float32: Float32Array): Int16Array {
    const int16 = new Int16Array(float32.length);
    for (let i = 0; i < float32.length; i++) {
      const s = Math.max(-1, Math.min(1, float32[i]));
      int16[i] = s < 0 ? s * 0x8000 : s * 0x7fff;
    }
    return int16;
  }

  private convertPCM16ToFloat32(pcm16Bytes: Uint8Array): Float32Array {
    const int16 = new Int16Array(
      pcm16Bytes.buffer,
      pcm16Bytes.byteOffset,
      pcm16Bytes.byteLength / 2,
    );
    const float32 = new Float32Array(int16.length);
    for (let i = 0; i < int16.length; i++) {
      float32[i] = int16[i] / 32768.0;
    }
    return float32;
  }
}
