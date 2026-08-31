// The key form's states: loading, no key on file, key on
// file (provider + last4 only), validate-and-save with a 422 surfacing the
// provider's reason, delete, and the banner shown when the API client routed a
// 409 llm_key_required here.
//
// The API module is mocked (the network contract has its own tests in
// lib/api.test.ts); ApiRequestError stays real so the page's instanceof checks
// run. useAuthContext needs no provider: the page renders fine on the
// context's default value.

import "@testing-library/jest-dom/vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { ApiRequestError } from "@/lib/api";
import SettingsPage from "./page";

const searchParams = vi.hoisted(() => ({ value: "" }));
vi.mock("next/navigation", () => ({
  useSearchParams: () => new URLSearchParams(searchParams.value),
}));

const api = vi.hoisted(() => ({
  getLLMKey: vi.fn(),
  putLLMKey: vi.fn(),
  deleteLLMKey: vi.fn(),
}));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    getLLMKey: api.getLLMKey,
    putLLMKey: api.putLLMKey,
    deleteLLMKey: api.deleteLLMKey,
  };
});

const noKey = () =>
  Promise.reject(new ApiRequestError(404, "no llm key on file"));

beforeEach(() => {
  api.getLLMKey.mockReset();
  api.putLLMKey.mockReset();
  api.deleteLLMKey.mockReset();
  searchParams.value = "";
});

afterEach(() => {
  cleanup();
});

describe("SettingsPage key form", () => {
  it("shows a loading state until the stored-key probe answers", () => {
    api.getLLMKey.mockReturnValue(new Promise(() => {})); // never settles
    render(<SettingsPage />);
    expect(screen.getByText("Loading…")).toBeInTheDocument();
  });

  it("renders the no-key state with a disabled save button and Gemini disabled", async () => {
    api.getLLMKey.mockImplementation(noKey);
    render(<SettingsPage />);

    expect(
      await screen.findByText(/No key on file — chat is disabled/),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Validate & save" }),
    ).toBeDisabled(); // nothing typed yet
    expect(
      screen.getByRole("option", { name: /Gemini \(coming soon\)/ }),
    ).toBeDisabled();
    expect(screen.getByRole("option", { name: "OpenAI" })).toBeEnabled();
  });

  it("shows provider and last4 only for a stored key", async () => {
    api.getLLMKey.mockResolvedValue({ provider: "openai", last4: "ab42" });
    render(<SettingsPage />);

    expect(await screen.findByText(/ab42/)).toBeInTheDocument();
    expect(screen.getByText(/key on file/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Delete" })).toBeInTheDocument();
    // With a key stored, saving replaces it.
    expect(
      screen.getByRole("button", { name: "Validate & replace" }),
    ).toBeInTheDocument();
  });

  it("validates and saves a typed key, then reports success with its last4", async () => {
    api.getLLMKey.mockImplementation(noKey);
    api.putLLMKey.mockResolvedValue({ provider: "openai", last4: "0042" });
    const user = userEvent.setup();
    render(<SettingsPage />);
    await screen.findByText(/No key on file/);

    await user.type(screen.getByLabelText("API key"), "sk-new-key-0042");
    const save = screen.getByRole("button", { name: "Validate & save" });
    expect(save).toBeEnabled();
    await user.click(save);

    await waitFor(() =>
      expect(api.putLLMKey).toHaveBeenCalledWith("openai", "sk-new-key-0042"),
    );
    expect(
      await screen.findByText("Key validated and saved."),
    ).toBeInTheDocument();
    expect(screen.getByText(/0042/)).toBeInTheDocument();
    // The password input is cleared: the key is never kept around client-side.
    expect(screen.getByLabelText("API key")).toHaveValue("");
  });

  it("surfaces a 422's provider reason as the form error", async () => {
    api.getLLMKey.mockImplementation(noKey);
    api.putLLMKey.mockRejectedValue(
      new ApiRequestError(422, "Incorrect API key provided: sk-bad."),
    );
    const user = userEvent.setup();
    render(<SettingsPage />);
    await screen.findByText(/No key on file/);

    await user.type(screen.getByLabelText("API key"), "sk-bad");
    await user.click(screen.getByRole("button", { name: "Validate & save" }));

    expect(
      await screen.findByText("Incorrect API key provided: sk-bad."),
    ).toBeInTheDocument();
    // Still no key on file; the form did not pretend the save happened.
    expect(screen.queryByText("Key validated and saved.")).toBeNull();
  });

  it("deletes the stored key and returns to the no-key state", async () => {
    api.getLLMKey.mockResolvedValue({ provider: "openai", last4: "ab42" });
    api.deleteLLMKey.mockResolvedValue(undefined);
    const user = userEvent.setup();
    render(<SettingsPage />);

    await user.click(await screen.findByRole("button", { name: "Delete" }));

    await waitFor(() => expect(api.deleteLLMKey).toHaveBeenCalledTimes(1));
    expect(await screen.findByText(/No key on file/)).toBeInTheDocument();
  });

  it("explains the redirect when routed here by a 409 llm_key_required", async () => {
    searchParams.value = "reason=llm_key_required";
    api.getLLMKey.mockImplementation(noKey);
    render(<SettingsPage />);

    expect(
      await screen.findByText(/Chat needs an LLM API key on file/),
    ).toBeInTheDocument();
  });

  it("shows no banner without the routed reason", async () => {
    api.getLLMKey.mockImplementation(noKey);
    render(<SettingsPage />);
    await screen.findByText(/No key on file/);
    expect(screen.queryByText(/Chat needs an LLM API key on file/)).toBeNull();
  });
});
