import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { ResourceList } from "@/components/resource-list"
import { Button } from "@/components/ui/button"
import { byName } from "@/lib/utils"
import { iamRolesQueryOptions } from "@/queries/iam"

export const Route = createFileRoute("/_auth/iam/(roles)/list-roles/")({
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...iamRolesQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [{ title: "Roles | IAM | Mulga" }],
  }),
  component: Roles,
})

function Roles() {
  const { data } = useSuspenseQuery(iamRolesQueryOptions)

  const roles = (data.Roles ?? []).toSorted(byName((r) => r.RoleName))

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

      <ResourceList
        empty="No IAM roles found."
        getKey={(role) => role.RoleName}
        items={roles}
        renderItem={(role, roleName) => (
          <ListCard
            params={{ roleName }}
            subtitle={role.Arn ?? ""}
            title={roleName}
            to="/iam/list-roles/$roleName"
          />
        )}
      />
    </>
  )
}
