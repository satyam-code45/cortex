"use client";

// The refresh control: "last refreshed X ago" plus a button that becomes a
// countdown when the server answers 429. The server owns the cooldown; this
// component only renders retry_after_seconds — it never computes its own.

import { useEffect, useRef, useState } from "react";

import { Button } from "@/components/ui/button";
import { formatCountdown, humanizeDate } from "@/lib/humanize";

export function RefreshButton({
  lastRefreshed,
  cooldownSeconds,
  refreshing,
  onRefresh,
  onCooldownEnd,
}: {
  lastRefreshed: string | null;
  // Seconds remaining, from the 429's retry_after_seconds; null = no cooldown.
  cooldownSeconds: number | null;
  refreshing: boolean;
  onRefresh: () => void;
  // Called when the countdown reaches zero, so the owner clears its
  // cooldownSeconds back to null. Without that reset, a later 429 carrying
  // the *same* number (two clicks straddling one crawl both ceil to 900)
  // would not be a prop change, and the countdown would never restart.
  onCooldownEnd?: () => void;
}) {
  // Local ticking copy of the server's figure, so the label counts down
  // without polling the server. Keyed by the prop and reset during render
  // when it changes, so the ticking effect never sets state synchronously.
  const [state, setState] = useState<{
    key: number | null;
    remaining: number | null;
  }>({ key: cooldownSeconds, remaining: cooldownSeconds });
  if (state.key !== cooldownSeconds) {
    setState({ key: cooldownSeconds, remaining: cooldownSeconds });
  }
  // Latest callback without re-arming the interval on parent re-renders.
  const onCooldownEndRef = useRef(onCooldownEnd);
  useEffect(() => {
    onCooldownEndRef.current = onCooldownEnd;
  }, [onCooldownEnd]);

  useEffect(() => {
    if (cooldownSeconds === null) return;
    const timer = setInterval(() => {
      setState((prev) => {
        if (prev.remaining === null || prev.remaining <= 1) {
          clearInterval(timer);
          if (prev.remaining !== null) onCooldownEndRef.current?.();
          return { ...prev, remaining: null };
        }
        return { ...prev, remaining: prev.remaining - 1 };
      });
    }, 1000);
    return () => clearInterval(timer);
  }, [cooldownSeconds]);

  const remaining = state.key === cooldownSeconds ? state.remaining : cooldownSeconds;
  const coolingDown = remaining !== null;
  return (
    <div className="flex items-center gap-3">
      {lastRefreshed && (
        <span className="text-xs text-muted-foreground">
          last refreshed {humanizeDate(lastRefreshed)}
        </span>
      )}
      <Button
        variant="outline"
        size="sm"
        disabled={refreshing || coolingDown}
        onClick={onRefresh}
      >
        {coolingDown
          ? `Refresh in ${formatCountdown(remaining)}`
          : refreshing
            ? "Refreshing…"
            : "Refresh"}
      </Button>
    </div>
  );
}
