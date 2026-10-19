import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { ResourceList } from "@/components/resource-list"
import { Button } from "@/components/ui/button"
import { byName } from "@/lib/utils"
import { iamUsersQueryOptions } from "@/queries/iam"

export const Route = createFileRoute("/_auth/iam/(users)/list-users/")({
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...iamUsersQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [{ title: "Users | IAM | Mulga" }],
  }),
  component: Users,
})

function Users() {
  const { data } = useSuspenseQuery(iamUsersQueryOptions)

  const users = (data.Users ?? []).toSorted(byName((u) => u.UserName))

  return (
    <>
      <PageHeading
        actions={
          <Link to="/iam/create-user">
            <Button>Create User</Button>
          </Link>
        }
        title="Users"
      />

      <ResourceList
        empty="No IAM users found."
        getKey={(user) => user.UserName}
        items={users}
        renderItem={(user, userName) => (
          <ListCard
            params={{ userName }}
            subtitle={user.UserId ?? ""}
            title={userName}
            to="/iam/list-users/$userName"
          />
        )}
      />
    </>
  )
}
