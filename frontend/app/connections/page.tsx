"use client";

// Connections: per-user source connections. Jira and Notion connect by
// pasting a key — validated live against the provider before anything is
// stored — and Gmail connects with one OAuth click. With no connections (or
// the demo toggle on) every question runs against the demo workspace; with any
// connection, ONLY the connected sources are searched, never demo data.

import { Suspense, useCallback, useEffect, useState } from "react";
import { useSearchParams } from "next/navigation";

import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import {
  ApiRequestError,
  deleteConnection,
  getConnections,
  gmailConnectUrl,
  putConnectionWrites,
  putConnectionsMode,
  putJiraConnection,
  putNotionConnection,
} from "@/lib/api";
import type {
  ConnectionSourceStatus,
  ConnectionsInfo,
  SourceName,
} from "@/lib/types";

// Messages for the query params the Gmail callback redirects back with.
const callbackErrors: Record<string, string> = {
  no_refresh_token:
    "Google returned no refresh token. Remove Cortex's access at myaccount.google.com/permissions, then connect again.",
  gmail_validation_failed:
    "Connected to Google, but the mailbox could not be read. Try again.",
  access_denied: "Google access was declined — nothing was connected.",
  gmail_send_not_granted:
    "Gmail is connected for reading, but permission to send was not granted — so sending stays off. Try again and allow the “send email on your behalf” permission.",
};

// CallbackBanner renders the ?connected= / ?error= result of the Gmail flow.
// The success banner is additionally gated on the live card state, so a
// disconnect on this same page doesn't leave a banner contradicting the card
// beside it.
function CallbackBanner({ gmailConnected }: { gmailConnected: boolean }) {
  const params = useSearchParams();
  const connected = params.get("connected");
  const error = params.get("error");
  if (params.get("writes_enabled") === "gmail" && gmailConnected) {
    return (
      <p className="rounded-md bg-muted px-3 py-2 text-sm">
        Cortex can now propose emails from this mailbox. Every one waits for your
        approval before it is sent.
      </p>
    );
  }
  if (connected === "gmail" && gmailConnected) {
    return (
      <p className="rounded-md bg-muted px-3 py-2 text-sm">
        Gmail connected.
      </p>
    );
  }
  if (error) {
    return (
      <p className="rounded-md bg-muted px-3 py-2 text-sm text-destructive">
        {callbackErrors[error] ?? `Gmail connection failed: ${error}`}
      </p>
    );
  }
  return null;
}

// identityLine renders the display facts a connection stored — never
// credentials; the API does not return them.
function identityLine(identity?: Record<string, string>): string {
  if (!identity) return "";
  return [
    identity.site_url,
    identity.account_name,
    identity.bot_name,
    identity.workspace_name,
    identity.email,
  ]
    .filter(Boolean)
    .join(" · ");
}

// writeConsequence says, in plain words, what enabling writes lets Cortex do as
// this account.
//
// Deliberately concrete and per-source, because the consequence differs and the
// user is the one who bears it. A generic "allow write access" tells nobody that
// their Jira account is what will appear as the author of a new ticket.
const writeConsequence: Record<SourceName, string> = {
  jira: "Cortex will be able to create issues, change fields, comment, and move issues through the workflow — all as this Jira account, so your name appears as the author.",
  notion:
    "Cortex will be able to add content to pages shared with the integration, and create new pages under them. It never edits or removes existing content.",
  gmail:
    "Cortex will be able to send email from this mailbox. Recipients see it as coming from you.",
};

// ConnectedRow shows a connected (or errored) source's identity, the writes
// switch, and the disconnect control.
function ConnectedRow({
  source,
  status,
  onDisconnect,
  onSetWrites,
  writesError,
}: {
  source: SourceName;
  status: ConnectionSourceStatus;
  onDisconnect: (source: SourceName) => void;
  onSetWrites: (source: SourceName, enabled: boolean) => void;
  writesError?: string;
}) {
  return (
    <div className="space-y-2 rounded-md border px-3 py-2">
      <div className="flex items-center justify-between gap-2">
        <div className="min-w-0">
          <p className="truncate text-sm">
            {status.status === "error" ? (
              <span className="font-medium text-destructive">
                Needs reconnecting
              </span>
            ) : (
              <span className="font-medium">Connected</span>
            )}{" "}
            <span className="text-muted-foreground">
              {identityLine(status.identity)}
            </span>
          </p>
          {status.status === "error" && status.last_error && (
            <p className="truncate text-sm text-muted-foreground">
              {status.last_error}
            </p>
          )}
        </div>
        <Button
          variant="destructive"
          size="sm"
          onClick={() => onDisconnect(source)}
        >
          Disconnect
        </Button>
      </div>

      {/* The writes switch is offered only on a working connection: turning it
          on runs a live permission check, which cannot pass against a credential
          that has already stopped working. */}
      {status.status === "connected" && (
        <div className="border-t pt-2">
          <div className="flex items-start justify-between gap-3">
            <div className="min-w-0">
              <p className="text-sm font-medium">
                {status.writes_enabled
                  ? "Cortex can propose changes"
                  : "Read-only"}
              </p>
              <p className="mt-0.5 text-xs text-muted-foreground">
                {status.writes_enabled
                  ? "Every change still waits for your approval before it happens — nothing is ever carried out on its own."
                  : writeConsequence[source]}
              </p>
            </div>
            <Button
              variant={status.writes_enabled ? "outline" : "secondary"}
              size="sm"
              className="shrink-0"
              onClick={() => onSetWrites(source, !status.writes_enabled)}
            >
              {status.writes_enabled ? "Turn off" : "Allow changes"}
            </Button>
          </div>
          {writesError && (
            <p className="mt-1 text-xs text-destructive">{writesError}</p>
          )}
        </div>
      )}
    </div>
  );
}

export default function ConnectionsPage() {
  // undefined = still loading; null = load failed (loadError says why).
  const [info, setInfo] = useState<ConnectionsInfo | null | undefined>(
    undefined,
  );
  const [loadError, setLoadError] = useState<string | null>(null);

  // Jira form.
  const [jiraBaseUrl, setJiraBaseUrl] = useState("");
  const [jiraEmail, setJiraEmail] = useState("");
  const [jiraToken, setJiraToken] = useState("");
  const [jiraSaving, setJiraSaving] = useState(false);
  const [jiraError, setJiraError] = useState<string | null>(null);

  // Notion form.
  const [notionToken, setNotionToken] = useState("");
  const [notionSaving, setNotionSaving] = useState(false);
  const [notionError, setNotionError] = useState<string | null>(null);

  const [modeError, setModeError] = useState<string | null>(null);

  const load = useCallback(() => {
    getConnections().then(
      (next) => setInfo(next),
      (err: unknown) => {
        setInfo(null);
        setLoadError(
          err instanceof ApiRequestError
            ? err.message
            : "failed to load connections",
        );
      },
    );
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const saveJira = useCallback(async () => {
    setJiraError(null);
    setJiraSaving(true);
    try {
      await putJiraConnection(
        jiraBaseUrl.trim(),
        jiraEmail.trim(),
        jiraToken.trim(),
      );
      setJiraBaseUrl("");
      setJiraEmail("");
      setJiraToken("");
      load();
    } catch (err) {
      // A 422 carries Atlassian's own reason.
      setJiraError(
        err instanceof ApiRequestError ? err.message : "failed to connect Jira",
      );
    } finally {
      setJiraSaving(false);
    }
  }, [jiraBaseUrl, jiraEmail, jiraToken, load]);

  const saveNotion = useCallback(async () => {
    setNotionError(null);
    setNotionSaving(true);
    try {
      await putNotionConnection(notionToken.trim());
      setNotionToken("");
      load();
    } catch (err) {
      setNotionError(
        err instanceof ApiRequestError
          ? err.message
          : "failed to connect Notion",
      );
    } finally {
      setNotionSaving(false);
    }
  }, [notionToken, load]);

  const [disconnectError, setDisconnectError] = useState<string | null>(null);

  const disconnect = useCallback(
    async (source: SourceName) => {
      setDisconnectError(null);
      try {
        await deleteConnection(source);
      } catch (err) {
        // Still reload below — the cards should show whatever is true now —
        // but say why the click appeared to do nothing.
        setDisconnectError(
          err instanceof ApiRequestError
            ? `failed to disconnect ${source}: ${err.message}`
            : `failed to disconnect ${source}`,
        );
      }
      load();
    },
    [load],
  );

  // Per-source writes errors, keyed by source: a refused Jira toggle must not
  // print its message under the Gmail card.
  const [writesErrors, setWritesErrors] = useState<
    Partial<Record<SourceName, string>>
  >({});

  const setWrites = useCallback(
    async (source: SourceName, enabled: boolean) => {
      setWritesErrors((prev) => ({ ...prev, [source]: undefined }));

      // Gmail sending needs a scope only a fresh consent screen can grant, so
      // enabling it is a navigation to Google rather than a background request.
      // A person should see the permission named by Google before Cortex can
      // send mail as them.
      if (source === "gmail" && enabled) {
        window.location.assign(gmailConnectUrl(true));
        return;
      }
      try {
        const next = await putConnectionWrites(source, enabled);
        setInfo(next);
      } catch (err) {
        setWritesErrors((prev) => ({
          ...prev,
          [source]:
            err instanceof ApiRequestError
              ? err.message
              : `failed to change write access for ${source}`,
        }));
      }
    },
    [],
  );

  const setUseDemo = useCallback(async (useDemo: boolean) => {
    setModeError(null);
    try {
      const next = await putConnectionsMode(useDemo);
      setInfo(next);
    } catch (err) {
      setModeError(
        err instanceof ApiRequestError ? err.message : "failed to switch mode",
      );
    }
  }, []);

  const sources = info?.sources;
  const jira = sources?.jira;
  const notion = sources?.notion;
  const gmail = sources?.gmail;
  const hasConnections =
    !!sources &&
    Object.values(sources).some((s) => s && s.status !== "absent");

  return (
    <div className="flex justify-center overflow-y-auto p-6">
      <div className="flex w-full max-w-xl flex-col gap-4">
        <h1 className="text-lg font-semibold tracking-tight">Connections</h1>
        <Suspense>
          <CallbackBanner
            gmailConnected={info?.sources?.gmail?.status === "connected"}
          />
        </Suspense>
        {disconnectError && (
          <p className="text-sm text-destructive">{disconnectError}</p>
        )}

        {info === undefined ? (
          <p className="text-sm text-muted-foreground">Loading…</p>
        ) : info === null ? (
          <p className="text-sm text-destructive">{loadError}</p>
        ) : (
          <>
            <div className="rounded-md bg-muted px-3 py-2 text-sm">
              {info.mode === "none" ? (
                <p>
                  <span className="font-medium">No sources connected yet.</span>{" "}
                  Connect Jira, Notion or Gmail below to ask questions about
                  your own data
                  {info.demo_available
                    ? " — or try the demo workspace to see how it works first."
                    : "."}
                </p>
              ) : info.mode === "demo" ? (
                <p>
                  Questions currently run against the{" "}
                  <span className="font-medium">demo workspace</span>
                  {hasConnections
                    ? " (the demo toggle is on)."
                    : " — read-only sample data owned by whoever runs this deployment, not yours."}
                </p>
              ) : (
                <p>
                  Questions run against{" "}
                  <span className="font-medium">your connected sources</span>{" "}
                  only — demo data and the demo knowledge base are never mixed
                  in.
                </p>
              )}
              {/* The demo toggle is offered only where a demo workspace
                  exists: a deployment without one must not advertise a mode it
                  cannot enter. */}
              {info.demo_available && (
                <label className="mt-2 flex items-center gap-2">
                  <input
                    type="checkbox"
                    checked={info.use_demo_workspace}
                    onChange={(e) => void setUseDemo(e.target.checked)}
                  />
                  Use demo workspace
                  {info.demo_sources && info.demo_sources.length > 0 && (
                    <span className="text-muted-foreground">
                      (read-only: {info.demo_sources.join(", ")})
                    </span>
                  )}
                </label>
              )}
              {modeError && (
                <p className="mt-1 text-destructive">{modeError}</p>
              )}
            </div>

            <Card>
              <CardContent className="flex flex-col gap-4 p-6">
                <div>
                  <h2 className="text-sm font-medium">Jira</h2>
                  <p className="text-sm text-muted-foreground">
                    Paste an API token from{" "}
                    <a
                      href="https://id.atlassian.com/manage-profile/security/api-tokens"
                      target="_blank"
                      rel="noreferrer"
                      className="underline"
                    >
                      id.atlassian.com
                    </a>
                    . It is validated against your site before being stored
                    encrypted.
                  </p>
                </div>
                {jira && jira.status !== "absent" && (
                  <ConnectedRow
                    source="jira"
                    status={jira}
                    onDisconnect={disconnect}
                    onSetWrites={setWrites}
                    writesError={writesErrors.jira}
                  />
                )}
                <form
                  className="flex flex-col gap-3"
                  onSubmit={(e) => {
                    e.preventDefault();
                    void saveJira();
                  }}
                >
                  <label className="flex flex-col gap-1 text-sm">
                    Site URL
                    <Input
                      placeholder="https://your-site.atlassian.net"
                      value={jiraBaseUrl}
                      onChange={(e) => setJiraBaseUrl(e.target.value)}
                    />
                  </label>
                  <label className="flex flex-col gap-1 text-sm">
                    Email
                    <Input
                      type="email"
                      autoComplete="off"
                      placeholder="you@example.com"
                      value={jiraEmail}
                      onChange={(e) => setJiraEmail(e.target.value)}
                    />
                  </label>
                  <label className="flex flex-col gap-1 text-sm">
                    API token
                    <Input
                      type="password"
                      autoComplete="off"
                      value={jiraToken}
                      onChange={(e) => setJiraToken(e.target.value)}
                    />
                  </label>
                  {jiraError && (
                    <p className="text-sm text-destructive">{jiraError}</p>
                  )}
                  <div>
                    <Button
                      type="submit"
                      disabled={
                        jiraSaving ||
                        jiraBaseUrl.trim() === "" ||
                        jiraEmail.trim() === "" ||
                        jiraToken.trim() === ""
                      }
                    >
                      {jiraSaving
                        ? "Validating…"
                        : jira && jira.status !== "absent"
                          ? "Validate & replace"
                          : "Validate & connect"}
                    </Button>
                  </div>
                </form>
              </CardContent>
            </Card>

            <Card>
              <CardContent className="flex flex-col gap-4 p-6">
                <div>
                  <h2 className="text-sm font-medium">Notion</h2>
                  <p className="text-sm text-muted-foreground">
                    Paste an internal integration token. Remember to share the
                    pages you want searchable with the integration — it can
                    only see what is shared with it.
                  </p>
                </div>
                {notion && notion.status !== "absent" && (
                  <ConnectedRow
                    source="notion"
                    status={notion}
                    onDisconnect={disconnect}
                    onSetWrites={setWrites}
                    writesError={writesErrors.notion}
                  />
                )}
                <form
                  className="flex flex-col gap-3"
                  onSubmit={(e) => {
                    e.preventDefault();
                    void saveNotion();
                  }}
                >
                  <label className="flex flex-col gap-1 text-sm">
                    Integration token
                    <Input
                      type="password"
                      autoComplete="off"
                      placeholder="ntn_…"
                      value={notionToken}
                      onChange={(e) => setNotionToken(e.target.value)}
                    />
                  </label>
                  {notionError && (
                    <p className="text-sm text-destructive">{notionError}</p>
                  )}
                  <div>
                    <Button
                      type="submit"
                      disabled={notionSaving || notionToken.trim() === ""}
                    >
                      {notionSaving
                        ? "Validating…"
                        : notion && notion.status !== "absent"
                          ? "Validate & replace"
                          : "Validate & connect"}
                    </Button>
                  </div>
                </form>
              </CardContent>
            </Card>

            <Card>
              <CardContent className="flex flex-col gap-4 p-6">
                <div>
                  <h2 className="text-sm font-medium">Gmail</h2>
                  <p className="text-sm text-muted-foreground">
                    One click: Google asks for read-only mail access and Cortex
                    stores a refresh token encrypted. No password or API key to
                    paste.
                  </p>
                </div>
                {gmail && gmail.status !== "absent" && (
                  <ConnectedRow
                    source="gmail"
                    status={gmail}
                    onDisconnect={disconnect}
                    onSetWrites={setWrites}
                    writesError={writesErrors.gmail}
                  />
                )}
                <div>
                  <Button
                    onClick={() => window.location.assign(gmailConnectUrl())}
                  >
                    {gmail?.status === "error"
                      ? "Reconnect Gmail"
                      : gmail?.status === "connected"
                        ? "Reconnect Gmail"
                        : "Connect Gmail"}
                  </Button>
                </div>
              </CardContent>
            </Card>
          </>
        )}
      </div>
    </div>
  );
}
