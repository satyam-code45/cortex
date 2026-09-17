import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { Header } from "@/components/nav/Header";
import type { Me } from "@/lib/types";

// The Sources entry is a capability, not decoration: the page browses the
// indexed corpus, and a deployment without one answers its Refresh with 503.
// So the nav must not offer it unless the server says an index exists — and
// must not offer it *provisionally* either, because a tab that appears and then
// vanishes is worse than one that arrives a moment late.

vi.mock("next/navigation", () => ({
  usePathname: () => "/",
}));

vi.mock("@/lib/api", () => ({
  logout: vi.fn(),
}));

const me: Me = {
  email: "someone@example.com",
  name: "Someone",
  avatar_url: "",
  has_llm_key: true,
};

function navLabels(): string[] {
  return screen
    .getAllByRole("link")
    .map((link) => link.textContent?.trim() ?? "")
    .filter(Boolean);
}

describe("Header navigation", () => {
  it("omits Sources while the capability is still unknown", () => {
    render(<Header me={me} />);
    expect(navLabels()).not.toContain("Sources");
  });

  it("omits Sources on a deployment with no index", () => {
    render(<Header me={me} indexingAvailable={false} />);
    expect(navLabels()).not.toContain("Sources");
  });

  it("shows Sources once the server reports an index", () => {
    render(<Header me={me} indexingAvailable />);
    expect(navLabels()).toContain("Sources");
  });

  it("always shows the entries that need no capability", () => {
    render(<Header me={me} indexingAvailable={false} />);
    const labels = navLabels();
    for (const label of ["Chat", "Actions", "Connections"]) {
      expect(labels).toContain(label);
    }
  });
});
