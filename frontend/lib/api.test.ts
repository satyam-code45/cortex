// The API client's cross-cutting auth routing.
//
// A 401 anywhere routes to /login (except the signed-out probe the login page
// itself makes), a 409 llm_key_required anywhere routes to settings, and a
// 429's retry_after_seconds is surfaced for the refresh countdown. The routing
// is observed through setAuthRouting, which returns the previous handlers so
// tests restore them.

import { afterEach, describe, expect, it, vi } from "vitest";

import {
  ApiRequestError,
  getMe,
  listConversations,
  refreshDocuments,
  sendChat,
  setAuthRouting,
  type AuthRouting,
} from "./api";

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

// installRouting swaps in observable handlers; restore() puts the previous
// ones back so no test leaks routing into another.
function installRouting() {
  const onUnauthorized = vi.fn();
  const onLLMKeyRequired = vi.fn();
  const prev = setAuthRouting({ onUnauthorized, onLLMKeyRequired });
  return {
    onUnauthorized,
    onLLMKeyRequired,
    restore: () => setAuthRouting(prev),
  };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("auth routing", () => {
  it("routes a 401 to the login handler and still rejects", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => jsonResponse(401, { error: "unauthorized" })),
    );
    const routing = installRouting();
    try {
      await expect(listConversations()).rejects.toMatchObject({
        name: "ApiRequestError",
        status: 401,
      });
      expect(routing.onUnauthorized).toHaveBeenCalledTimes(1);
      expect(routing.onLLMKeyRequired).not.toHaveBeenCalled();
    } finally {
      routing.restore();
    }
  });

  it("does not route the login page's own /api/auth/me probe", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => jsonResponse(401, { error: "unauthorized" })),
    );
    const routing = installRouting();
    try {
      await expect(getMe()).rejects.toMatchObject({ status: 401 });
      expect(routing.onUnauthorized).not.toHaveBeenCalled();
    } finally {
      routing.restore();
    }
  });

  it("routes a 409 llm_key_required to settings", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => jsonResponse(409, { error: "llm_key_required" })),
    );
    const routing = installRouting();
    try {
      await expect(sendChat("hello")).rejects.toMatchObject({
        status: 409,
        message: "llm_key_required",
      });
      expect(routing.onLLMKeyRequired).toHaveBeenCalledTimes(1);
      expect(routing.onUnauthorized).not.toHaveBeenCalled();
    } finally {
      routing.restore();
    }
  });

  it("does not route a 409 that is not llm_key_required", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => jsonResponse(409, { error: "some other conflict" })),
    );
    const routing = installRouting();
    try {
      await expect(sendChat("hello")).rejects.toMatchObject({ status: 409 });
      expect(routing.onLLMKeyRequired).not.toHaveBeenCalled();
    } finally {
      routing.restore();
    }
  });

  it("setAuthRouting returns the previous handlers for restoration", () => {
    const mine: AuthRouting = {
      onUnauthorized: vi.fn(),
      onLLMKeyRequired: vi.fn(),
    };
    const before = setAuthRouting(mine);
    const got = setAuthRouting(before); // restore, capturing what was active
    expect(got).toBe(mine);
  });
});

describe("error surface", () => {
  it("carries retry_after_seconds from a 429 refresh cooldown", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        jsonResponse(429, {
          error: "refresh_cooldown",
          retry_after_seconds: 540,
        }),
      ),
    );
    const routing = installRouting();
    try {
      await refreshDocuments();
      expect.unreachable("a 429 must reject");
    } catch (err) {
      expect(err).toBeInstanceOf(ApiRequestError);
      const apiErr = err as ApiRequestError;
      expect(apiErr.status).toBe(429);
      expect(apiErr.message).toBe("refresh_cooldown");
      expect(apiErr.retryAfterSeconds).toBe(540);
    } finally {
      routing.restore();
    }
  });

  it("leaves retryAfterSeconds null when the body has none", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => jsonResponse(500, { error: "boom" })),
    );
    const routing = installRouting();
    try {
      await expect(listConversations()).rejects.toMatchObject({
        status: 500,
        retryAfterSeconds: null,
      });
    } finally {
      routing.restore();
    }
  });
});

describe("request shape", () => {
  it("sends credentials:'include' so the session cookie travels", async () => {
    const fetchMock = vi.fn(async () => jsonResponse(200, []));
    vi.stubGlobal("fetch", fetchMock);

    await expect(listConversations()).resolves.toEqual([]);

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as unknown as [
      string,
      RequestInit,
    ];
    expect(url).toMatch(/\/api\/conversations$/);
    expect(init.credentials).toBe("include");
  });
});
