import type { DBInstance } from "@aws-sdk/client-rds"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Link, useNavigate } from "@tanstack/react-router"
import { useState } from "react"

import { ErrorBanner } from "@/components/error-banner"
import { PageHeading } from "@/components/page-heading"
import { StateBadge } from "@/components/state-badge"
import { Button } from "@/components/ui/button"
import {
  useRebootDBInstance,
  useStartDBInstance,
  useStopDBInstance,
} from "@/mutations/rds"
import { rdsDBInstancesQueryOptions } from "@/queries/rds"
import { canDelete, canReboot, canStart, canStop } from "@/types/rds"

import { DeleteDBInstanceDialog } from "./delete-db-instance-dialog"

interface DeleteTarget {
  identifier: string
  deletionProtection: boolean
}

function engineLabel(instance: DBInstance): string {
  return [instance.Engine, instance.EngineVersion].filter(Boolean).join(" ")
}

export function DBInstancesListPage() {
  const navigate = useNavigate()
  const { data } = useSuspenseQuery(rdsDBInstancesQueryOptions)
  const startInstance = useStartDBInstance()
  const stopInstance = useStopDBInstance()
  const rebootInstance = useRebootDBInstance()
  const [deleteTarget, setDeleteTarget] = useState<DeleteTarget | null>(null)

  const instances = data.DBInstances ?? []
  const lifecycleError =
    startInstance.error ?? stopInstance.error ?? rebootInstance.error

  return (
    <>
      <PageHeading
        actions={
          <Button
            onClick={async () =>
              await navigate({ to: "/rds/create-db-instance" })
            }
          >
            Create Database
          </Button>
        }
        title="Databases"
      />

      {lifecycleError && (
        <ErrorBanner
          error={lifecycleError}
          msg="Failed to change the DB instance state."
        />
      )}

      {instances.length > 0 ? (
        <div className="overflow-x-auto rounded-lg border bg-card">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b text-left text-muted-foreground">
                <th className="px-4 py-2 font-medium">Identifier</th>
                <th className="px-4 py-2 font-medium">Engine</th>
                <th className="px-4 py-2 font-medium">Class</th>
                <th className="px-4 py-2 font-medium">Status</th>
                <th className="px-4 py-2 font-medium">Endpoint</th>
                <th className="px-4 py-2 font-medium">Storage</th>
                <th className="px-4 py-2 font-medium">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {instances.map((instance) => {
                const id = instance.DBInstanceIdentifier
                if (!id) {
                  return null
                }
                const status = instance.DBInstanceStatus
                return (
                  <tr
                    className="cursor-pointer border-b transition-colors last:border-0 hover:bg-accent"
                    key={id}
                    onClick={async () =>
                      await navigate({
                        to: "/rds/describe-db-instances/$id",
                        params: { id },
                      })
                    }
                  >
                    <td className="px-4 py-2 font-medium">
                      <Link
                        className="text-primary hover:underline"
                        onClick={(e) => e.stopPropagation()}
                        params={{ id }}
                        to="/rds/describe-db-instances/$id"
                      >
                        {id}
                      </Link>
                    </td>
                    <td className="px-4 py-2">{engineLabel(instance)}</td>
                    <td className="px-4 py-2">{instance.DBInstanceClass}</td>
                    <td className="px-4 py-2">
                      <StateBadge state={status} />
                    </td>
                    <td className="px-4 py-2 font-mono text-xs">
                      {instance.Endpoint?.Address ?? "—"}
                    </td>
                    <td className="px-4 py-2">
                      {instance.AllocatedStorage
                        ? `${instance.AllocatedStorage} GiB`
                        : "—"}
                    </td>
                    <td className="space-x-2 px-4 py-2 text-right">
                      <Button
                        disabled={!canStart(status) || startInstance.isPending}
                        onClick={(e) => {
                          e.stopPropagation()
                          startInstance.mutate(id)
                        }}
                        size="sm"
                        variant="outline"
                      >
                        Start
                      </Button>
                      <Button
                        disabled={!canStop(status) || stopInstance.isPending}
                        onClick={(e) => {
                          e.stopPropagation()
                          stopInstance.mutate(id)
                        }}
                        size="sm"
                        variant="outline"
                      >
                        Stop
                      </Button>
                      <Button
                        disabled={
                          !canReboot(status) || rebootInstance.isPending
                        }
                        onClick={(e) => {
                          e.stopPropagation()
                          rebootInstance.mutate(id)
                        }}
                        size="sm"
                        variant="outline"
                      >
                        Reboot
                      </Button>
                      <Button
                        disabled={!canDelete(status)}
                        onClick={(e) => {
                          e.stopPropagation()
                          setDeleteTarget({
                            identifier: id,
                            deletionProtection:
                              instance.DeletionProtection ?? false,
                          })
                        }}
                        size="sm"
                        variant="destructive"
                      >
                        Delete
                      </Button>
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </div>
      ) : (
        <p className="text-muted-foreground">No DB instances found.</p>
      )}

      {deleteTarget && (
        <DeleteDBInstanceDialog
          dbInstanceIdentifier={deleteTarget.identifier}
          deletionProtection={deleteTarget.deletionProtection}
          onOpenChange={(open) => !open && setDeleteTarget(null)}
          open={true}
        />
      )}
    </>
  )
}
