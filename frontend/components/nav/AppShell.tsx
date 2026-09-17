"use client";

// AppShell owns the session for the whole app: one /api/auth/me probe, shared
// down through context so pages don't re-fetch identity on every navigation.
// It lives in the root layout, which stays a server component.

import { createContext, useContext, useEffect, useState } from "react";

import { Header } from "@/components/nav/Header";
import { getConnections } from "@/lib/api";
import { useAuth, type AuthState } from "@/lib/useAuth";

const AuthContext = createContext<AuthState>({
  me: null,
  loading: true,
  refresh: () => {},
});

// useAuthContext reads the shared session state anywhere under AppShell.
export function useAuthContext(): AuthState {
  return useContext(AuthContext);
}

export function AppShell({ children }: { children: React.ReactNode }) {
  const auth = useAuth();

  // Whether this deployment has an indexed corpus decides whether the Sources
  // nav entry is shown. That is the capability the page needs — not whether a
  // demo workspace exists, which is a weaker condition: a demo configured
  // without the server's OpenAI key has no index, and a Sources tab there leads
  // to an empty list whose Refresh answers 503.
  //
  // undefined until the probe lands, and the entry is added only on a positive
  // answer. Defaulting to true instead would show the tab on every load of a
  // deployment that has no index and then take it away — and a tab vanishing
  // under the cursor is worse than one that arrives a moment late.
  const [indexingAvailable, setIndexingAvailable] = useState<boolean | undefined>(undefined);
  useEffect(() => {
    if (!auth.me) return;
    let cancelled = false;
    void getConnections()
      .then((info) => {
        if (!cancelled) setIndexingAvailable(info.indexing_available);
      })
      .catch(() => {
        // Leave it unknown: a failed probe is not evidence either way, and the
        // nav stays as it is rather than flickering on a transient error.
      });
    return () => {
      cancelled = true;
    };
  }, [auth.me]);

  return (
    <AuthContext.Provider value={auth}>
      <div className="flex h-full flex-col">
        <Header me={auth.me} indexingAvailable={indexingAvailable} />
        <div className="min-h-0 flex-1">{children}</div>
      </div>
    </AuthContext.Provider>
  );
}
