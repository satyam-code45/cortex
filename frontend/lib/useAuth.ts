"use client";

// useAuth loads the signed-in identity once per mount. A 401 is not handled
// here: /api/auth/me is exempt from the client's global 401 routing (it is the
// probe the login page itself makes), so this hook does its own redirect —
// every page that uses it requires a session.

import { useCallback, useEffect, useState } from "react";

import { ApiRequestError, getMe } from "./api";
import type { Me } from "./types";

export interface AuthState {
  // null while loading or when signed out (the redirect is already underway).
  me: Me | null;
  loading: boolean;
  // refresh re-fetches after something identity-adjacent changes (a key was
  // added or deleted, say — has_llm_key gates the chat input).
  refresh: () => void;
}

export function useAuth(): AuthState {
  const [me, setMe] = useState<Me | null>(null);
  const [loading, setLoading] = useState(true);

  const refresh = useCallback(() => {
    getMe().then(
      (identity) => {
        setMe(identity);
        setLoading(false);
      },
      (err: unknown) => {
        setMe(null);
        setLoading(false);
        if (
          err instanceof ApiRequestError &&
          err.status === 401 &&
          window.location.pathname !== "/login"
        ) {
          window.location.assign("/login");
        }
      },
    );
  }, []);

  useEffect(refresh, [refresh]);

  return { me, loading, refresh };
}
