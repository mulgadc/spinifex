import type { KnowledgeBaseRetrievalResult } from "@aws-sdk/client-bedrock-agent-runtime"
import { useState } from "react"

import { Button } from "@/components/ui/button"
import { Label } from "@/components/ui/label"
import { Textarea } from "@/components/ui/textarea"
import { useRetrieve } from "@/mutations/bedrockAgentRuntime"

interface RetrieveTesterProps {
  knowledgeBaseId: string
}

function resultSourceLocation(
  result: KnowledgeBaseRetrievalResult,
): string | undefined {
  return result.location?.s3Location?.uri
}

export function RetrieveTester({ knowledgeBaseId }: RetrieveTesterProps) {
  const [query, setQuery] = useState("")
  const retrieve = useRetrieve()

  async function handleSubmit() {
    if (query.trim().length === 0) {
      return
    }
    try {
      await retrieve.mutateAsync({ knowledgeBaseId, query })
    } catch {
      // error shown via retrieve.error
    }
  }

  const results = retrieve.data?.retrievalResults ?? []

  return (
    <div className="rounded-lg border bg-card p-4">
      <h2 className="mb-2 font-semibold">Retrieve tester</h2>
      <p className="mb-4 text-sm text-muted-foreground">
        Run a similarity query against this knowledge base and inspect the
        returned chunks.
      </p>

      <form
        className="space-y-2"
        onSubmit={(event) => {
          event.preventDefault()
          void handleSubmit()
        }}
      >
        <Label htmlFor="retrieve-query">Query</Label>
        <Textarea
          id="retrieve-query"
          onChange={(event) => {
            setQuery(event.target.value)
          }}
          placeholder="What would you like to ask this knowledge base?"
          rows={3}
          value={query}
        />
        <Button
          disabled={retrieve.isPending || query.trim().length === 0}
          type="submit"
        >
          {retrieve.isPending ? "Retrieving…" : "Retrieve"}
        </Button>
      </form>

      {retrieve.error && (
        <p className="mt-4 text-sm text-destructive">
          {retrieve.error.message}
        </p>
      )}

      {retrieve.isSuccess && (
        <div className="mt-4 space-y-3">
          <h3 className="text-sm font-medium">
            {results.length} chunk{results.length === 1 ? "" : "s"} returned
          </h3>
          {results.length > 0 ? (
            results.map((result, index) => {
              const location = resultSourceLocation(result)
              const key = result.documentId ?? `${location ?? "chunk"}-${index}`
              return (
                <div className="rounded-md border p-3 text-sm" key={key}>
                  <div className="mb-1 flex items-center justify-between gap-2">
                    <span className="font-mono text-xs text-muted-foreground">
                      {location ?? "unknown source"}
                    </span>
                    {result.score !== undefined && (
                      <span className="rounded-full bg-muted px-2 py-0.5 text-xs">
                        score {result.score.toFixed(4)}
                      </span>
                    )}
                  </div>
                  <p className="whitespace-pre-wrap">
                    {result.content?.text ?? "(no content)"}
                  </p>
                </div>
              )
            })
          ) : (
            <p className="text-muted-foreground">
              No chunks matched this query.
            </p>
          )}
        </div>
      )}
    </div>
  )
}
