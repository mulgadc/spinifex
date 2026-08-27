import type { TokenUsage } from "@aws-sdk/client-bedrock-runtime"

// "pending" is in flight; "warming-up" and "error" are settled failure states
// a turn can be retried from. The list is mutated in place — the id a turn is
// created with is the id later patched, never replaced — so a later streaming
// swap can keep appending text to the same assistant turn.
export type TurnStatus = "complete" | "pending" | "warming-up" | "error"

export interface Turn {
  id: string
  role: "user" | "assistant"
  text: string
  status: TurnStatus
  usage?: TokenUsage
  errorMessage?: string
}
