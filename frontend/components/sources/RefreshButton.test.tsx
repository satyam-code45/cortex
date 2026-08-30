// TEST-7.5 — the refresh countdown renders from the server's
// retry_after_seconds (REQ-7.5). The server owns the cooldown; the component
// only displays and ticks the figure it was handed — it never computes one.

import "@testing-library/jest-dom/vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { RefreshButton } from "./RefreshButton";

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe("RefreshButton", () => {
  it("is an enabled Refresh button when there is no cooldown", () => {
    const onRefresh = vi.fn();
    render(
      <RefreshButton
        lastRefreshed={null}
        cooldownSeconds={null}
        refreshing={false}
        onRefresh={onRefresh}
      />,
    );
    const button = screen.getByRole("button", { name: "Refresh" });
    expect(button).toBeEnabled();
    fireEvent.click(button);
    expect(onRefresh).toHaveBeenCalledTimes(1);
  });

  it("renders the countdown from retry_after_seconds and disables itself", () => {
    render(
      <RefreshButton
        lastRefreshed={null}
        cooldownSeconds={95}
        refreshing={false}
        onRefresh={vi.fn()}
      />,
    );
    const button = screen.getByRole("button", { name: "Refresh in 1:35" });
    expect(button).toBeDisabled();
  });

  it("ticks the countdown down and re-enables when it reaches zero", () => {
    vi.useFakeTimers();
    render(
      <RefreshButton
        lastRefreshed={null}
        cooldownSeconds={95}
        refreshing={false}
        onRefresh={vi.fn()}
      />,
    );

    act(() => {
      vi.advanceTimersByTime(60_000);
    });
    expect(
      screen.getByRole("button", { name: "Refresh in 0:35" }),
    ).toBeDisabled();

    act(() => {
      vi.advanceTimersByTime(40_000);
    });
    expect(screen.getByRole("button", { name: "Refresh" })).toBeEnabled();
  });

  it("shows Refreshing… while a refresh is in flight", () => {
    render(
      <RefreshButton
        lastRefreshed={null}
        cooldownSeconds={null}
        refreshing
        onRefresh={vi.fn()}
      />,
    );
    expect(screen.getByRole("button", { name: "Refreshing…" })).toBeDisabled();
  });

  it("shows the humanized last-refreshed stamp", () => {
    const fiveMinAgo = new Date(Date.now() - 5 * 60 * 1000).toISOString();
    render(
      <RefreshButton
        lastRefreshed={fiveMinAgo}
        cooldownSeconds={null}
        refreshing={false}
        onRefresh={vi.fn()}
      />,
    );
    expect(screen.getByText("last refreshed 5m ago")).toBeInTheDocument();
  });
});
