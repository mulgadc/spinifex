import { useSuspenseQuery } from "@tanstack/react-query"
import { useState } from "react"

import {
  type DataColumn,
  DataTable,
  type SortState,
} from "@/components/data-table"
import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { ErrorBanner } from "@/components/error-banner"
import { Button } from "@/components/ui/button"
import { arnResourceId } from "@/lib/utils"
import { useDeregisterTaskDefinition } from "@/mutations/ecs"
import { ecsTaskDefinitionsQueryOptions } from "@/queries/ecs"

// DeregisterTaskDefinition requires the explicit "family:revision", which is
// the ARN's resource part.
interface TaskDefinitionRow {
  arn: string
  id: string
}

function taskDefinitionColumns(
  onDeregister: (id: string) => void,
): DataColumn<TaskDefinitionRow>[] {
  return [
    {
      id: "family",
      header: "Family : Revision",
      sortValue: (row) => row.id,
      cell: (row) => <span className="font-mono text-xs">{row.id}</span>,
    },
    {
      id: "actions",
      header: "Actions",
      align: "end",
      cell: (row) => (
        <Button
          onClick={() => {
            onDeregister(row.id)
          }}
          size="sm"
          variant="destructive"
        >
          Deregister
        </Button>
      ),
    },
  ]
}

// TaskDefinitionsList renders the account-scoped task definitions.
export function TaskDefinitionsList() {
  const { data: arns } = useSuspenseQuery(ecsTaskDefinitionsQueryOptions)
  const deregister = useDeregisterTaskDefinition()
  const [target, setTarget] = useState<string | null>(null)
  const [sort, setSort] = useState<SortState>()
  const columns = taskDefinitionColumns(setTarget)

  function handleDeregister() {
    if (!target) {
      return
    }

    deregister.mutate(target, {
      onSuccess: () => {
        setTarget(null)
      },
    })
  }

  return (
    <>
      {deregister.isError && (
        <ErrorBanner
          error={deregister.error}
          msg="Failed to deregister task definition."
        />
      )}

      <DataTable
        caption="Task definitions"
        columns={columns}
        empty="No task definitions found."
        getRowId={(row) => row.arn}
        onSortChange={setSort}
        rows={arns.map((arn) => ({ arn, id: arnResourceId(arn) }))}
        sort={sort}
      />

      <DeleteConfirmationDialog
        confirmLabel="Deregister"
        description={`This marks task definition "${target}" INACTIVE. Existing tasks keep running; no new tasks can use it.`}
        isPending={deregister.isPending}
        onConfirm={handleDeregister}
        onOpenChange={(open) => {
          if (!open) {
            setTarget(null)
          }
        }}
        open={target !== null}
        pendingLabel="Deregistering…"
        title="Deregister task definition"
      />
    </>
  )
}
