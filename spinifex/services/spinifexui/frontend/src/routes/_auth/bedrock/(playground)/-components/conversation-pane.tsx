import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { cn } from "@/lib/utils"

import type { Turn } from "./types"

interface ConversationPaneProps {
  turns: Turn[]
  onRetry: (turnId: string) => void
}

function TurnBubble({
  turn,
  onRetry,
}: {
  turn: Turn
  onRetry: (turnId: string) => void
}) {
  const isUser = turn.role === "user"

  return (
    <div
      className={cn(
        "max-w-2xl rounded-lg border p-3 text-sm",
        isUser ? "ml-auto bg-muted" : "mr-auto bg-card",
      )}
    >
      <div className="mb-1 flex items-center gap-2 text-xs text-muted-foreground">
        <span className="font-medium">{isUser ? "You" : "Assistant"}</span>
        {turn.status === "complete" && turn.usage && (
          <>
            <Badge className="text-[0.625rem]" variant="outline">
              {turn.usage.inputTokens ?? 0} in / {turn.usage.outputTokens ?? 0}{" "}
              out
            </Badge>
            <Badge className="text-[0.625rem]" variant="secondary">
              Included
            </Badge>
          </>
        )}
      </div>

      {turn.status === "pending" && (
        <p className="text-muted-foreground italic">Thinking…</p>
      )}

      {turn.status === "warming-up" && (
        <div className="space-y-2">
          <p className="text-tactical-amber">
            Model is warming up — retry in a moment.
          </p>
          <Button
            onClick={() => {
              onRetry(turn.id)
            }}
            size="sm"
            type="button"
            variant="outline"
          >
            Retry
          </Button>
        </div>
      )}

      {turn.status === "error" && (
        <div className="space-y-2">
          <p className="text-destructive">
            {turn.errorMessage ?? "Converse failed."}
          </p>
          <Button
            onClick={() => {
              onRetry(turn.id)
            }}
            size="sm"
            type="button"
            variant="outline"
          >
            Retry
          </Button>
        </div>
      )}

      {turn.status === "complete" && (
        <p className="break-words whitespace-pre-wrap">{turn.text}</p>
      )}
    </div>
  )
}

export function ConversationPane({ turns, onRetry }: ConversationPaneProps) {
  if (turns.length === 0) {
    return (
      <p className="text-sm text-muted-foreground">
        Send a message to start a conversation with the selected model.
      </p>
    )
  }

  return (
    <div className="space-y-3">
      {turns.map((turn) => (
        <TurnBubble key={turn.id} onRetry={onRetry} turn={turn} />
      ))}
    </div>
  )
}
