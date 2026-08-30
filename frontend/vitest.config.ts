import path from "node:path";
import { defineConfig } from "vitest/config";

export default defineConfig({
  // Vite 8's oxc transform handles .tsx JSX on its own (tsconfig's
  // jsx:"preserve" is Next's concern, not the test runner's); only the
  // "@/..." import alias needs wiring here.
  resolve: {
    alias: { "@": path.resolve(import.meta.dirname) },
  },
  test: {
    // Tests are generated in the Testing stage (/test-feature); the build
    // stage's gate must not fail just because they do not exist yet.
    passWithNoTests: true,
    // Two projects, two environments: pure functions (lib/) stay on node —
    // fast, no DOM globals leaking into parser tests — while component tests
    // (components/, app/) get jsdom (TEST-7.5).
    projects: [
      {
        extends: true,
        test: {
          name: "unit",
          environment: "node",
          include: ["lib/**/*.test.ts"],
        },
      },
      {
        extends: true,
        test: {
          name: "components",
          environment: "jsdom",
          include: ["components/**/*.test.tsx", "app/**/*.test.tsx"],
        },
      },
    ],
  },
});
