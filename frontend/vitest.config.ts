import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    // The parser under test (lib/citations.ts) is DOM-free, so the default
    // node environment is enough — no jsdom dependency.
    include: ["lib/**/*.test.ts"],
    // Tests are generated in the Testing stage (/test-feature); the build
    // stage's gate must not fail just because they do not exist yet.
    passWithNoTests: true,
  },
});
