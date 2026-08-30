"use client";

// The app-wide header: brand, Chat/Sources navigation, and the identity
// cluster (settings, avatar + name, logout, theme). Hidden on /login — that
// page has no session to render.

import Link from "next/link";
import { usePathname } from "next/navigation";

import { ThemeToggle } from "@/components/ThemeToggle";
import { Button } from "@/components/ui/button";
import { logout } from "@/lib/api";
import type { Me } from "@/lib/types";
import { cn } from "@/lib/utils";

const navItems = [
  { href: "/", label: "Chat" },
  { href: "/sources", label: "Sources" },
  { href: "/connections", label: "Connections" },
];

export function Header({ me }: { me: Me | null }) {
  const pathname = usePathname();
  if (pathname === "/login") return null;

  const signOut = async () => {
    try {
      await logout();
    } finally {
      // Hard navigation on purpose: it drops every piece of client state that
      // belonged to the session that just ended.
      window.location.assign("/login");
    }
  };

  return (
    <header className="flex h-12 shrink-0 items-center gap-4 border-b px-4">
      <Link href="/" className="text-sm font-semibold tracking-tight">
        Cortex
      </Link>
      <nav className="flex items-center gap-1">
        {navItems.map((item) => (
          <Link
            key={item.href}
            href={item.href}
            className={cn(
              "rounded-md px-2.5 py-1 text-sm transition-colors",
              pathname === item.href
                ? "bg-muted font-medium text-foreground"
                : "text-muted-foreground hover:bg-muted hover:text-foreground",
            )}
          >
            {item.label}
          </Link>
        ))}
      </nav>
      <div className="ml-auto flex items-center gap-2">
        <ThemeToggle />
        {me && (
          <>
            <Link
              href="/settings"
              className={cn(
                "rounded-md px-2.5 py-1 text-sm transition-colors",
                pathname === "/settings"
                  ? "bg-muted font-medium text-foreground"
                  : "text-muted-foreground hover:bg-muted hover:text-foreground",
              )}
            >
              Settings
            </Link>
            <span className="flex items-center gap-2" title={me.email}>
              {me.avatar_url ? (
                // Google avatar URLs are remote and per-user; next/image would
                // demand a remotePatterns allowlist for lh3.googleusercontent.com
                // for zero optimization benefit on a 24px avatar.
                // eslint-disable-next-line @next/next/no-img-element
                <img
                  src={me.avatar_url}
                  alt=""
                  className="size-6 rounded-full"
                  referrerPolicy="no-referrer"
                />
              ) : (
                <span className="flex size-6 items-center justify-center rounded-full bg-muted text-xs font-medium uppercase">
                  {(me.name || me.email).slice(0, 1)}
                </span>
              )}
              <span className="hidden text-sm text-muted-foreground sm:inline">
                {me.name || me.email}
              </span>
            </span>
            <Button variant="ghost" size="sm" onClick={signOut}>
              Log out
            </Button>
          </>
        )}
      </div>
    </header>
  );
}
