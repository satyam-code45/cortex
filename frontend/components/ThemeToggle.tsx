"use client";

// Light/dark toggle. The saved choice is applied before first paint by the
// inline script in app/layout.tsx; this button only flips the class and the
// stored value.
//
// Stateless on purpose: both icons are rendered and the `dark:` variant picks
// one, so the component never has to read the DOM into state — which would
// either mismatch the server render or need a post-mount effect.

import { Moon, Sun } from "lucide-react";

import { Button } from "@/components/ui/button";

export function ThemeToggle() {
  const toggle = () => {
    const next = !document.documentElement.classList.contains("dark");
    document.documentElement.classList.toggle("dark", next);
    try {
      localStorage.setItem("theme", next ? "dark" : "light");
    } catch {
      // Private windows: the toggle still works for this page view.
    }
  };

  return (
    <Button
      variant="ghost"
      size="icon"
      onClick={toggle}
      aria-label="Toggle dark mode"
    >
      <Sun className="hidden size-4 dark:block" />
      <Moon className="size-4 dark:hidden" />
    </Button>
  );
}
