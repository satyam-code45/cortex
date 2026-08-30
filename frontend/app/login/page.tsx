"use client";

// The signed-out landing: one button, straight to Google. The callback error
// param comes back here (?error=not_allowed, ?error=access_denied, ...) so a
// refused sign-in explains itself instead of silently bouncing.

import { Suspense } from "react";
import { useSearchParams } from "next/navigation";

import { Card, CardContent } from "@/components/ui/card";
import { googleLoginUrl } from "@/lib/api";

// The known error codes get a human sentence; anything else is shown as-is —
// an OAuth error code beats "something went wrong".
const errorMessages: Record<string, string> = {
  not_allowed:
    "This Cortex instance restricts sign-in to an allowlist, and your Google account is not on it.",
  access_denied: "Sign-in was cancelled on Google's consent screen.",
};

function LoginError() {
  // useSearchParams needs a Suspense boundary; keeping it in this leaf keeps
  // the rest of the page static.
  const params = useSearchParams();
  const code = params.get("error");
  if (!code) return null;
  return (
    <p className="rounded-md bg-destructive/10 px-3 py-2 text-sm text-destructive">
      {errorMessages[code] ?? `Sign-in failed: ${code}`}
    </p>
  );
}

export default function LoginPage() {
  return (
    <div className="flex h-full items-center justify-center p-4">
      <Card className="w-full max-w-sm">
        <CardContent className="flex flex-col gap-6 p-8">
          <div className="flex flex-col gap-1 text-center">
            <h1 className="text-xl font-semibold tracking-tight">Cortex</h1>
            <p className="text-sm text-muted-foreground">
              An agent that investigates Jira, Notion and Gmail — with
              citations and a full execution trace.
            </p>
          </div>
          <Suspense>
            <LoginError />
          </Suspense>
          {/* A plain anchor, not a fetch: signing in IS navigating away. */}
          <a
            href={googleLoginUrl()}
            className="inline-flex h-10 items-center justify-center gap-2 rounded-lg border bg-background text-sm font-medium transition-colors hover:bg-muted"
          >
            <GoogleMark />
            Continue with Google
          </a>
          <p className="text-center text-xs text-muted-foreground">
            You bring your own LLM API key; your conversations and spend are
            yours. The demo workspace is shared and read-only.
          </p>
        </CardContent>
      </Card>
    </div>
  );
}

// Google's "G", inline so the login page needs no asset pipeline.
function GoogleMark() {
  return (
    <svg viewBox="0 0 24 24" aria-hidden className="size-4">
      <path
        fill="#4285F4"
        d="M23.5 12.27c0-.85-.08-1.66-.22-2.45H12v4.64h6.45a5.52 5.52 0 0 1-2.39 3.62v3h3.87c2.26-2.09 3.57-5.16 3.57-8.81Z"
      />
      <path
        fill="#34A853"
        d="M12 24c3.24 0 5.96-1.07 7.93-2.91l-3.87-3c-1.07.72-2.44 1.14-4.06 1.14-3.12 0-5.77-2.11-6.71-4.95H1.29v3.1A12 12 0 0 0 12 24Z"
      />
      <path
        fill="#FBBC05"
        d="M5.29 14.28a7.2 7.2 0 0 1 0-4.56v-3.1H1.29a12 12 0 0 0 0 10.76l4-3.1Z"
      />
      <path
        fill="#EA4335"
        d="M12 4.77c1.76 0 3.34.6 4.58 1.79l3.44-3.44A11.97 11.97 0 0 0 12 0 12 12 0 0 0 1.29 6.62l4 3.1C6.23 6.88 8.88 4.77 12 4.77Z"
      />
    </svg>
  );
}
