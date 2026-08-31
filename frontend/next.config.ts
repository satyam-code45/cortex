import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  // next dev otherwise writes agent-docs overlay files (AGENTS.md and friends)
  // into frontend/ on every run. This repository maintains its own agent
  // instructions at the root, so a generated per-directory copy would compete
  // with them and churn in every diff.
  agentRules: false,
};

export default nextConfig;
