"use client";

// Sidebar list of conversations: create, select, newest-updated first (the
// API already orders them).

import { Plus } from "lucide-react";

import { Button } from "@/components/ui/button";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Skeleton } from "@/components/ui/skeleton";
import type { Conversation } from "@/lib/types";
import { cn } from "@/lib/utils";

interface Props {
  // null while the first load is in flight.
  conversations: Conversation[] | null;
  activeId: string | null;
  onSelect: (id: string) => void;
  onNew: () => void;
}

export function ConversationList({
  conversations,
  activeId,
  onSelect,
  onNew,
}: Props) {
  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="p-2">
        <Button
          variant="outline"
          size="sm"
          className="w-full justify-start gap-2"
          onClick={onNew}
        >
          <Plus className="size-4" />
          New conversation
        </Button>
      </div>
      <ScrollArea className="min-h-0 flex-1">
        <nav className="flex flex-col gap-1 p-2 pt-0">
          {conversations === null ? (
            <>
              <Skeleton className="h-8 w-full" />
              <Skeleton className="h-8 w-full" />
              <Skeleton className="h-8 w-full" />
            </>
          ) : conversations.length === 0 ? (
            <p className="px-2 py-4 text-sm text-muted-foreground">
              No conversations yet. Ask something to start one.
            </p>
          ) : (
            conversations.map((c) => (
              <button
                key={c.id}
                onClick={() => onSelect(c.id)}
                className={cn(
                  "truncate rounded-md px-2 py-1.5 text-left text-sm hover:bg-accent",
                  c.id === activeId && "bg-accent font-medium",
                )}
                title={c.title ?? "Untitled"}
              >
                {c.title ?? "Untitled"}
              </button>
            ))
          )}
        </nav>
      </ScrollArea>
    </div>
  );
}
