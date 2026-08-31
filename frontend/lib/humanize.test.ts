// The countdown formatter behind the refresh button's disabled
// label. The server sends whole seconds (retry_after_seconds); the UI shows
// m:ss.

import { describe, expect, it } from "vitest";

import { formatCountdown, humanizeDate } from "./humanize";

describe("formatCountdown", () => {
  it.each([
    [0, "0:00"],
    [1, "0:01"],
    [59, "0:59"],
    [60, "1:00"],
    [95, "1:35"],
    [540, "9:00"],
    [899, "14:59"],
    [900, "15:00"], // the default INDEX_REFRESH_COOLDOWN, fresh
  ])("renders %d seconds as %s", (seconds, want) => {
    expect(formatCountdown(seconds)).toBe(want);
  });

  it("clamps negatives to 0:00 rather than rendering nonsense", () => {
    expect(formatCountdown(-5)).toBe("0:00");
  });

  it("floors fractional seconds", () => {
    expect(formatCountdown(61.9)).toBe("1:01");
  });
});

describe("humanizeDate", () => {
  it("renders a recent timestamp relatively", () => {
    const fiveMinAgo = new Date(Date.now() - 5 * 60 * 1000).toISOString();
    expect(humanizeDate(fiveMinAgo)).toBe("5m ago");
  });

  it("renders the near-now and empty cases safely", () => {
    expect(humanizeDate(new Date().toISOString())).toBe("just now");
    expect(humanizeDate(null)).toBe("");
    expect(humanizeDate("not-a-date")).toBe("");
  });
});
