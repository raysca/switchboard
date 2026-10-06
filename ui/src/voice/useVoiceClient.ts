import { useEffect, useRef, useState, useCallback } from "react";
import type { ApiClient } from "../api/client";
import {
  VoiceClient,
  type ConnectOptions,
  type VoiceClientState,
} from "./VoiceClient";

export function useVoiceClient(apiClient: ApiClient) {
  const clientRef = useRef<VoiceClient | null>(null);

  if (!clientRef.current) {
    clientRef.current = new VoiceClient(apiClient);
  }

  const [state, setState] = useState<VoiceClientState>(() =>
    clientRef.current!.getState(),
  );

  useEffect(() => {
    const client = clientRef.current!;
    const unsubscribe = client.subscribe((nextState) => {
      setState(nextState);
    });

    return () => {
      unsubscribe();
      client.destroy();
    };
  }, []);

  const connect = useCallback((options: ConnectOptions) => {
    return clientRef.current?.connect(options);
  }, []);

  const disconnect = useCallback(() => {
    return clientRef.current?.disconnect();
  }, []);

  const toggleMute = useCallback(() => {
    clientRef.current?.toggleMute();
  }, []);

  const clearLogs = useCallback(() => {
    clientRef.current?.clearLogs();
  }, []);

  return {
    ...state,
    connect,
    disconnect,
    toggleMute,
    clearLogs,
    client: clientRef.current,
  };
}
