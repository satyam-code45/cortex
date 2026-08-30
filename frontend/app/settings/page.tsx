"use client";

// Settings: the bring-your-own-key form. The key is validated against the
// provider with one live call before it is stored (encrypted, server-side);
// afterwards only the provider and last4 ever come back.

import { Suspense, useCallback, useEffect, useState } from "react";
import { useSearchParams } from "next/navigation";

import { useAuthContext } from "@/components/nav/AppShell";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import {
  ApiRequestError,
  deleteLLMKey,
  getLLMKey,
  putLLMKey,
} from "@/lib/api";
import type { LLMKeyInfo } from "@/lib/types";

// KeyRequiredBanner explains why the user landed here when the API client
// routed a 409 llm_key_required to this page.
function KeyRequiredBanner() {
  const params = useSearchParams();
  if (params.get("reason") !== "llm_key_required") return null;
  return (
    <p className="rounded-md bg-muted px-3 py-2 text-sm">
      Chat needs an LLM API key on file — add yours below. It funds only your
      own conversations.
    </p>
  );
}

export default function SettingsPage() {
  const { refresh: refreshAuth } = useAuthContext();

  // undefined = still loading; null = no key on file.
  const [stored, setStored] = useState<LLMKeyInfo | null | undefined>(
    undefined,
  );
  const [provider, setProvider] = useState("openai");
  const [key, setKey] = useState("");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);

  useEffect(() => {
    getLLMKey().then(
      (info) => setStored(info),
      (err: unknown) => {
        if (err instanceof ApiRequestError && err.status === 404) {
          setStored(null);
          return;
        }
        setStored(null);
        setError(
          err instanceof ApiRequestError
            ? err.message
            : "failed to load key state",
        );
      },
    );
  }, []);

  const save = useCallback(async () => {
    setError(null);
    setSaved(false);
    setSaving(true);
    try {
      const info = await putLLMKey(provider, key.trim());
      setStored(info);
      setKey("");
      setSaved(true);
      refreshAuth(); // has_llm_key just changed; the chat gate reads it.
    } catch (err) {
      // A 422 carries the provider's own reason (invalid key, "coming soon").
      setError(
        err instanceof ApiRequestError ? err.message : "failed to save key",
      );
    } finally {
      setSaving(false);
    }
  }, [key, provider, refreshAuth]);

  const remove = useCallback(async () => {
    setError(null);
    setSaved(false);
    try {
      await deleteLLMKey();
      setStored(null);
      refreshAuth();
    } catch (err) {
      setError(
        err instanceof ApiRequestError ? err.message : "failed to delete key",
      );
    }
  }, [refreshAuth]);

  return (
    <div className="flex justify-center overflow-y-auto p-6">
      <div className="flex w-full max-w-xl flex-col gap-4">
        <h1 className="text-lg font-semibold tracking-tight">Settings</h1>
        <Suspense>
          <KeyRequiredBanner />
        </Suspense>

        <Card>
          <CardContent className="flex flex-col gap-4 p-6">
            <div>
              <h2 className="text-sm font-medium">LLM API key</h2>
              <p className="text-sm text-muted-foreground">
                Your key makes the model calls for your questions. It is
                validated live, stored encrypted, and never shown again — only
                its last four characters.
              </p>
            </div>

            {stored === undefined ? (
              <p className="text-sm text-muted-foreground">Loading…</p>
            ) : stored ? (
              <div className="flex items-center justify-between rounded-md border px-3 py-2">
                <p className="text-sm">
                  <span className="font-medium capitalize">
                    {stored.provider}
                  </span>{" "}
                  key on file{" "}
                  <span className="font-mono text-muted-foreground">
                    ····{stored.last4}
                  </span>
                </p>
                <Button variant="destructive" size="sm" onClick={remove}>
                  Delete
                </Button>
              </div>
            ) : (
              <p className="text-sm text-muted-foreground">
                No key on file — chat is disabled until you add one.
              </p>
            )}

            <form
              className="flex flex-col gap-3"
              onSubmit={(e) => {
                e.preventDefault();
                void save();
              }}
            >
              <label className="flex flex-col gap-1 text-sm">
                Provider
                <select
                  value={provider}
                  onChange={(e) => setProvider(e.target.value)}
                  className="h-8 rounded-lg border bg-background px-2 text-sm"
                >
                  <option value="openai">OpenAI</option>
                  <option value="gemini" disabled>
                    Gemini (coming soon)
                  </option>
                </select>
              </label>
              <label className="flex flex-col gap-1 text-sm">
                API key
                <Input
                  type="password"
                  autoComplete="off"
                  placeholder="sk-…"
                  value={key}
                  onChange={(e) => setKey(e.target.value)}
                />
              </label>
              {error && <p className="text-sm text-destructive">{error}</p>}
              {saved && (
                <p className="text-sm text-muted-foreground">
                  Key validated and saved.
                </p>
              )}
              <div>
                <Button type="submit" disabled={saving || key.trim() === ""}>
                  {saving
                    ? "Validating…"
                    : stored
                      ? "Validate & replace"
                      : "Validate & save"}
                </Button>
              </div>
            </form>
          </CardContent>
        </Card>
      </div>
    </div>
  );
}
