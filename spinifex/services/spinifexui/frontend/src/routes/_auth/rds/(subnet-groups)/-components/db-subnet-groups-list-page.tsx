import { useSuspenseQuery } from "@tanstack/react-query"
import { Link, useNavigate } from "@tanstack/react-router"
import { useState } from "react"

import { PageHeading } from "@/components/page-heading"
import { StateBadge } from "@/components/state-badge"
import { Button } from "@/components/ui/button"
import { rdsSubnetGroupsQueryOptions } from "@/queries/rds"

import { DeleteDBSubnetGroupDialog } from "./delete-db-subnet-group-dialog"

export function DBSubnetGroupsListPage() {
  const navigate = useNavigate()
  const { data } = useSuspenseQuery(rdsSubnetGroupsQueryOptions)
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null)

  const groups = data.DBSubnetGroups ?? []

  return (
    <>
      <PageHeading
        actions={
          <Button
            onClick={async () =>
              await navigate({ to: "/rds/create-db-subnet-group" })
            }
          >
            Create Subnet Group
          </Button>
        }
        title="DB Subnet Groups"
      />

      {groups.length > 0 ? (
        <div className="overflow-x-auto rounded-lg border bg-card">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b text-left text-muted-foreground">
                <th className="px-4 py-2 font-medium">Name</th>
                <th className="px-4 py-2 font-medium">VPC</th>
                <th className="px-4 py-2 font-medium">Subnets</th>
                <th className="px-4 py-2 font-medium">Status</th>
                <th className="px-4 py-2 font-medium">Description</th>
                <th className="px-4 py-2 font-medium">
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {groups.map((group) => {
                const name = group.DBSubnetGroupName
                if (!name) {
                  return null
                }
                return (
                  <tr
                    className="cursor-pointer border-b transition-colors last:border-0 hover:bg-accent"
                    key={name}
                    onClick={async () =>
                      await navigate({
                        to: "/rds/describe-db-subnet-groups/$name",
                        params: { name },
                      })
                    }
                  >
                    <td className="px-4 py-2 font-medium">
                      <Link
                        className="text-primary hover:underline"
                        onClick={(e) => e.stopPropagation()}
                        params={{ name }}
                        to="/rds/describe-db-subnet-groups/$name"
                      >
                        {name}
                      </Link>
                    </td>
                    <td className="px-4 py-2 font-mono text-xs">
                      {group.VpcId ?? "—"}
                    </td>
                    <td className="px-4 py-2">{group.Subnets?.length ?? 0}</td>
                    <td className="px-4 py-2">
                      <StateBadge state={group.SubnetGroupStatus} />
                    </td>
                    <td className="px-4 py-2 text-muted-foreground">
                      {group.DBSubnetGroupDescription}
                    </td>
                    <td className="px-4 py-2 text-right">
                      <Button
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
        <p className="text-muted-foreground">
          No DB subnet groups found. Without one, a database is placed in the
          account&apos;s default VPC subnet.
        </p>
      )}

      {deleteTarget && (
        <DeleteDBSubnetGroupDialog
          dbSubnetGroupName={deleteTarget}
          onOpenChange={(open) => !open && setDeleteTarget(null)}
          open={true}
        />
      )}
    </>
  )
}
