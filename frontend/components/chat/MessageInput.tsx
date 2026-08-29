"use client";

// The input box. Disabled while a run is in flight: one investigation at a
// time keeps the trace panel unambiguous, and the backend queues runs per
// conversation anyway.

import { SendHorizonal } from "lucide-react";
import { type FormEvent, useState } from "react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";

export function MessageInput({
  onSend,
  disabled,
}: {
  onSend: (text: string) => void;
  disabled: boolean;
}) {
  const [text, setText] = useState("");

  const submit = (e: FormEvent) => {
    e.preventDefault();
    const trimmed = text.trim();
    if (trimmed === "" || disabled) return;
    onSend(trimmed);
    setText("");
  };

  return (
    <form onSubmit={submit} className="flex gap-2 border-t p-3">
      <Input
        value={text}
        onChange={(e) => setText(e.target.value)}
        placeholder={disabled ? "Investigating…" : "Ask about your projects…"}
        disabled={disabled}
        autoFocus
      />
      <Button type="submit" size="icon" disabled={disabled || !text.trim()}>
        <SendHorizonal className="size-4" />
      </Button>
    </form>
  );
}
