"use client";

// AppShell owns the session for the whole app: one /api/auth/me probe, shared
// down through context so pages don't re-fetch identity on every navigation.
// It lives in the root layout, which stays a server component.

import { createContext, useContext } from "react";

import { Header } from "@/components/nav/Header";
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
  return (
    <AuthContext.Provider value={auth}>
      <div className="flex h-full flex-col">
        <Header me={auth.me} />
        <div className="min-h-0 flex-1">{children}</div>
      </div>
    </AuthContext.Provider>
  );
}
