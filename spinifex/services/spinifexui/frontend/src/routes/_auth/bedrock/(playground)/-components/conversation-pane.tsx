import type { RefObject } from "react"
import Markdown from "react-markdown"
import remarkGfm from "remark-gfm"

import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { cn } from "@/lib/utils"

import type { Turn } from "./types"

interface ConversationPaneProps {
  turns: Turn[]
  onRetry: (turnId: string) => void
  // Sentinel at the end of the thread; the parent scrolls it into view when
  // turns change so the newest message stays visible.
  bottomRef: RefObject<HTMLDivElement | null>
}

// Explicit element classes so assistant markdown reads well without depending
// on the typography plugin; the nested-code reset stops code blocks from
// double-painting the inline-code background.
const markdownComponents = {
  p: (props: { children?: React.ReactNode }) => (
    <p className="mb-2 leading-relaxed last:mb-0">{props.children}</p>
  ),
  ul: (props: { children?: React.ReactNode }) => (
    <ul className="mb-2 list-disc space-y-1 pl-5 last:mb-0">
      {props.children}
    </ul>
  ),
  ol: (props: { children?: React.ReactNode }) => (
    <ol className="mb-2 list-decimal space-y-1 pl-5 last:mb-0">
      {props.children}
    </ol>
  ),
  a: (props: { href?: string; children?: React.ReactNode }) => (
    <a
      className="text-primary underline"
      href={props.href}
      rel="noreferrer"
      target="_blank"
    >
      {props.children}
    </a>
  ),
  code: (props: { children?: React.ReactNode }) => (
    <code className="rounded bg-muted px-1 py-0.5 font-mono text-[0.85em]">
      {props.children}
    </code>
  ),
  pre: (props: { children?: React.ReactNode }) => (
    <pre className="mb-2 overflow-x-auto rounded bg-muted p-3 text-xs last:mb-0 [&>code]:bg-transparent [&>code]:p-0">
      {props.children}
    </pre>
  ),
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

      {turn.status === "complete" &&
        (isUser ? (
          <p className="break-words whitespace-pre-wrap">{turn.text}</p>
        ) : (
          <div className="break-words">
            <Markdown
              components={markdownComponents}
              remarkPlugins={[remarkGfm]}
            >
              {turn.text}
            </Markdown>
          </div>
        ))}
    </div>
  )
}

export function ConversationPane({
  turns,
  onRetry,
  bottomRef,
}: ConversationPaneProps) {
  return (
    <div className="min-h-0 flex-1 overflow-y-auto">
      {turns.length === 0 ? (
        <p className="text-sm text-muted-foreground">
          Send a message to start a conversation with the selected model.
        </p>
      ) : (
        <div className="space-y-3">
          {turns.map((turn) => (
            <TurnBubble key={turn.id} onRetry={onRetry} turn={turn} />
          ))}
          <div ref={bottomRef} />
        </div>
      )}
    </div>
  )
}
