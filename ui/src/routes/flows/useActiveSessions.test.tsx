import { describe, expect, it, vi } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import type { ApiClient } from "../../api/client";
import { useActiveSessions } from "./useActiveSessions";

const wrapper = ({ children }: { children: ReactNode }) => (
  <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>{children}</QueryClientProvider>
);

describe("useActiveSessions", () => {
  it("asks for the active sessions, at the API's page size", async () => {
    const getSessions = vi.fn().mockResolvedValue([{ id: "s1" }]);
    const { result } = renderHook(() => useActiveSessions({ getSessions } as unknown as ApiClient, "key"), { wrapper });
    await waitFor(() => expect(result.current.data).toEqual([{ id: "s1" }]));
    expect(getSessions).toHaveBeenCalledWith({ status: "active", limit: 100 });
  });

  it("does nothing without a management key", () => {
    const getSessions = vi.fn();
    renderHook(() => useActiveSessions({ getSessions } as unknown as ApiClient, null), { wrapper });
    expect(getSessions).not.toHaveBeenCalled();
  });
});
