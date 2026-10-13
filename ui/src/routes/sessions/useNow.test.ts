import { afterEach, describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { useNow } from "./useNow";

afterEach(() => vi.useRealTimers());

describe("useNow", () => {
  it("ticks every second while running, and holds still when not", () => {
    vi.useFakeTimers();
    vi.setSystemTime(1_000_000);
    const running = renderHook(() => useNow(true));
    const idle = renderHook(() => useNow(false));
    const before = running.result.current;
    act(() => {
      vi.advanceTimersByTime(3000);
    });
    expect(running.result.current - before).toBeGreaterThanOrEqual(3000);
    expect(idle.result.current).toBe(1_000_000);
  });

  it("stops its timer when it stops running or unmounts", () => {
    vi.useFakeTimers();
    const { rerender, unmount } = renderHook(({ on }) => useNow(on), { initialProps: { on: true } });
    expect(vi.getTimerCount()).toBe(1);
    rerender({ on: false });
    expect(vi.getTimerCount()).toBe(0);
    rerender({ on: true });
    unmount();
    expect(vi.getTimerCount()).toBe(0);
  });
});
