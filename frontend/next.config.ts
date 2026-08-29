import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  // next dev otherwise writes AGENTS.md/CLAUDE.md into frontend/ on every run;
  // the repository has its own CLAUDE.md and does not want generated overlays.
  agentRules: false,
};

export default nextConfig;
