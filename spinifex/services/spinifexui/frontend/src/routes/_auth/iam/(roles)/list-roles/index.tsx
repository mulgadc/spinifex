import type { Role } from "@aws-sdk/client-iam"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { Button } from "@/components/ui/button"
import { iamRolesQueryOptions } from "@/queries/iam"

export const Route = createFileRoute("/_auth/iam/(roles)/list-roles/")({
  loader: async ({ context }) => {
    await context.queryClient.ensureQueryData(iamRolesQueryOptions)
  },
  head: () => ({
    meta: [{ title: "Roles | IAM | Mulga" }],
  }),
  component: Roles,
})

function Roles() {
  const { data } = useSuspenseQuery(iamRolesQueryOptions)

  const roles = (data.Roles ?? []).toSorted((a, b) => {
    const nameA = a.RoleName?.toLowerCase() ?? ""
    const nameB = b.RoleName?.toLowerCase() ?? ""
    return nameA.localeCompare(nameB)
  })

  return (
    <>
      <PageHeading
        actions={
          <Link to="/iam/create-role">
            <Button>Create Role</Button>
          </Link>
        }
        title="Roles"
      />

      {roles.length > 0 ? (
        <div className="space-y-4">
          {roles.map((role: Role) => {
            if (!role.RoleName) {
              return null
            }
            return (
              <ListCard
                key={role.RoleName}
                params={{ roleName: role.RoleName }}
                subtitle={role.Arn ?? ""}
                title={role.RoleName}
                to="/iam/list-roles/$roleName"
              />
            )
          })}
        </div>
      ) : (
        <p className="text-muted-foreground">No IAM roles found.</p>
      )}
    </>
  )
}
