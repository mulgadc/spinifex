import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { usePutImageTagMutability } from "@/mutations/ecr"

import { PushCommands } from "./push-commands"

interface RepositorySummaryProps {
  repositoryName: string
  repositoryUri: string | undefined
  imageTagMutability: string | undefined
}

// RepositorySummary renders the push-command snippet plus the tag-immutability
// control. An unset mutability defaults to MUTABLE, matching the backend.
export function RepositorySummary({
  repositoryName,
  repositoryUri,
  imageTagMutability,
}: RepositorySummaryProps) {
  const putMutability = usePutImageTagMutability()
  const isImmutable = imageTagMutability === "IMMUTABLE"
  const next = isImmutable ? "MUTABLE" : "IMMUTABLE"
  const toggleLabel = isImmutable ? "Make mutable" : "Make immutable"

  async function handleToggle() {
    try {
      await putMutability.mutateAsync({
        repositoryName,
        imageTagMutability: next,
      })
    } catch {
      // error shown via putMutability.error
    }
  }

  return (
    <div className="space-y-6">
      <PushCommands
        repositoryName={repositoryName}
        repositoryUri={repositoryUri}
      />

      <div className="rounded-lg border bg-card p-4">
        <div className="flex items-center justify-between">
          <div>
            <h2 className="text-sm font-medium">Tag immutability</h2>
            <p className="mt-1 text-xs text-muted-foreground">
              {isImmutable
                ? "Tags cannot be overwritten to point at a different image."
                : "Tags can be overwritten by a new push."}
            </p>
          </div>
          <Badge variant={isImmutable ? "default" : "outline"}>
            {isImmutable ? "Immutable" : "Mutable"}
          </Badge>
        </div>
        {putMutability.error && (
          <p className="mt-2 text-sm text-destructive">
            {putMutability.error.message}
          </p>
        )}
        <Button
          className="mt-3"
          disabled={putMutability.isPending}
          onClick={() => void handleToggle()}
          size="sm"
          variant="outline"
        >
          {putMutability.isPending ? "Saving…" : toggleLabel}
        </Button>
      </div>
    </div>
  )
}
