import { useQuery } from "@tanstack/react-query";
import type { ApiClient } from "../../api/client";

/** The calls in progress (the API returns at most 100), refreshed every 10 seconds. */
export function useActiveSessions(client: ApiClient, keyValue: string | null) {
  return useQuery({
    queryKey: ["sessions", "active"],
    queryFn: () => client.getSessions({ status: "active", limit: 100 }),
    enabled: !!keyValue,
    refetchInterval: 10_000,
    retry: false,
  });
}
