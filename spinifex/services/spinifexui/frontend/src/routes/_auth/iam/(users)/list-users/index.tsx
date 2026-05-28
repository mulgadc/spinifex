import type { User } from "@aws-sdk/client-iam"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { Button } from "@/components/ui/button"
import { iamUsersQueryOptions } from "@/queries/iam"

export const Route = createFileRoute("/_auth/iam/(users)/list-users/")({
  loader: async ({ context }) => {
    await context.queryClient.ensureQueryData(iamUsersQueryOptions)
  },
  head: () => ({
    meta: [{ title: "Users | IAM | Mulga" }],
  }),
  component: Users,
})

function Users() {
  const { data } = useSuspenseQuery(iamUsersQueryOptions)

  const users = (data.Users ?? []).toSorted((a, b) => {
    const nameA = a.UserName?.toLowerCase() ?? ""
    const nameB = b.UserName?.toLowerCase() ?? ""
    return nameA.localeCompare(nameB)
  })

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

      {users.length > 0 ? (
        <div className="space-y-4">
          {users.map((user: User) => {
            if (!user.UserName) {
              return null
            }
            return (
              <ListCard
                key={user.UserName}
                params={{ userName: user.UserName }}
                subtitle={user.UserId ?? ""}
                title={user.UserName}
                to="/iam/list-users/$userName"
              />
            )
          })}
        </div>
      ) : (
        <p className="text-muted-foreground">No IAM users found.</p>
      )}
    </>
  )
}
