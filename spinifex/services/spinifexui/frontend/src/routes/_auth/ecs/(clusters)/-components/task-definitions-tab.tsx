import { useSuspenseQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import { useState } from "react"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { ErrorBanner } from "@/components/error-banner"
import { Button } from "@/components/ui/button"
import { useDeregisterTaskDefinition } from "@/mutations/ecs"
import { ecsTaskDefinitionsQueryOptions } from "@/queries/ecs"

// familyRevision extracts the "family:revision" identifier from a task
// definition ARN; DeregisterTaskDefinition requires that explicit form.
function familyRevision(arn: string): string {
  const idx = arn.lastIndexOf("/")
  return idx === -1 ? arn : arn.slice(idx + 1)
}

export function TaskDefinitionsTab({ clusterName }: { clusterName: string }) {
  const { data: arns } = useSuspenseQuery(ecsTaskDefinitionsQueryOptions)
  const deregister = useDeregisterTaskDefinition()
  const [target, setTarget] = useState<string | null>(null)

  function handleDeregister() {
    if (!target) {
      return
    }
    deregister.mutate(target, { onSuccess: () => setTarget(null) })
  }

  return (
    <>
      <div className="mb-4 flex justify-end">
        <Link
          search={{ cluster: clusterName }}
          to="/ecs/register-task-definition"
        >
          <Button size="sm">Register task definition</Button>
        </Link>
      </div>

      {deregister.isError && (
        <ErrorBanner
          error={
            deregister.error instanceof Error ? deregister.error : undefined
          }
          msg="Failed to deregister task definition."
        />
      )}

      {arns.length === 0 ? (
        <p className="text-muted-foreground">No task definitions found.</p>
      ) : (
        <div className="overflow-x-auto rounded-lg border bg-card">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b text-left text-muted-foreground">
                <th className="px-4 py-2 font-medium">Family : Revision</th>
                <th className="px-4 py-2 font-medium">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {arns.map((arn: string) => {
                const id = familyRevision(arn)
                return (
                  <tr className="border-b last:border-0" key={arn}>
                    <td className="px-4 py-2 font-mono text-xs">{id}</td>
                    <td className="px-4 py-2 text-right">
                      <Button
                        onClick={() => setTarget(id)}
                        size="sm"
                        variant="destructive"
                      >
                        Deregister
                      </Button>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      )}

      <DeleteConfirmationDialog
        confirmLabel="Deregister"
        description={`This marks task definition "${target}" INACTIVE. Existing tasks keep running; no new tasks can use it.`}
        isPending={deregister.isPending}
        onConfirm={handleDeregister}
        onOpenChange={(open) => !open && setTarget(null)}
        open={target !== null}
        pendingLabel="Deregistering…"
        title="Deregister task definition"
      />
    </>
  )
}
