import { useSuspenseQuery } from "@tanstack/react-query"
import { Link, useNavigate } from "@tanstack/react-router"
import { useState } from "react"

import { PageHeading } from "@/components/page-heading"
import { Button } from "@/components/ui/button"
import { rdsParameterGroupsQueryOptions } from "@/queries/rds"
import { isDefaultParameterGroupName } from "@/types/rds"

import { DeleteDBParameterGroupDialog } from "./delete-db-parameter-group-dialog"

export function DBParameterGroupsListPage() {
  const navigate = useNavigate()
  const { data } = useSuspenseQuery(rdsParameterGroupsQueryOptions)
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null)

  const groups = data.DBParameterGroups ?? []

  return (
    <>
      <PageHeading
        actions={
          <Button
            onClick={async () =>
              await navigate({ to: "/rds/create-db-parameter-group" })
            }
          >
            Create Parameter Group
          </Button>
        }
        title="DB Parameter Groups"
      />

      {groups.length > 0 ? (
        <div className="overflow-x-auto rounded-lg border bg-card">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b text-left text-muted-foreground">
                <th className="px-4 py-2 font-medium">Name</th>
                <th className="px-4 py-2 font-medium">Family</th>
                <th className="px-4 py-2 font-medium">Type</th>
                <th className="px-4 py-2 font-medium">Description</th>
                <th className="px-4 py-2 font-medium">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {groups.map((group) => {
                const name = group.DBParameterGroupName
                if (!name) {
                  return null
                }
                const isDefault = isDefaultParameterGroupName(name)
                return (
                  <tr
                    className="cursor-pointer border-b transition-colors last:border-0 hover:bg-accent"
                    key={name}
                    onClick={async () =>
                      await navigate({
                        to: "/rds/describe-db-parameter-groups/$name",
                        params: { name },
                      })
                    }
                  >
                    <td className="px-4 py-2 font-medium">
                      <Link
                        className="text-primary hover:underline"
                        onClick={(e) => e.stopPropagation()}
                        params={{ name }}
                        to="/rds/describe-db-parameter-groups/$name"
                      >
                        {name}
                      </Link>
                    </td>
                    <td className="px-4 py-2 font-mono text-xs">
                      {group.DBParameterGroupFamily}
                    </td>
                    <td className="px-4 py-2 text-muted-foreground">
                      {isDefault ? "Default" : "Customer"}
                    </td>
                    <td className="px-4 py-2 text-muted-foreground">
                      {group.Description}
                    </td>
                    <td className="px-4 py-2 text-right">
                      <Button
                        disabled={isDefault}
                        onClick={(e) => {
                          e.stopPropagation()
                          setDeleteTarget(name)
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
        <p className="text-muted-foreground">No DB parameter groups found.</p>
      )}

      <p className="mt-4 text-xs text-muted-foreground">
        A default group is owned by the service. It is readable and attachable
        but cannot be modified, deleted or tagged — create your own group to
        change a value.
      </p>

      {deleteTarget && (
        <DeleteDBParameterGroupDialog
          dbParameterGroupName={deleteTarget}
          onOpenChange={(open) => !open && setDeleteTarget(null)}
          open={true}
        />
      )}
    </>
  )
}
