import type { Group } from "@aws-sdk/client-iam"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { Button } from "@/components/ui/button"
import { iamGroupsQueryOptions } from "@/queries/iam"

export const Route = createFileRoute("/_auth/iam/(groups)/list-groups/")({
  loader: async ({ context }) => {
    await context.queryClient.ensureQueryData(iamGroupsQueryOptions)
  },
  head: () => ({
    meta: [{ title: "Groups | IAM | Mulga" }],
  }),
  component: Groups,
})

function Groups() {
  const { data } = useSuspenseQuery(iamGroupsQueryOptions)

  const groups = (data.Groups ?? []).toSorted((a, b) => {
    const nameA = a.GroupName?.toLowerCase() ?? ""
    const nameB = b.GroupName?.toLowerCase() ?? ""
    return nameA.localeCompare(nameB)
  })

  return (
    <>
      <PageHeading
        actions={
          <Link to="/iam/create-group">
            <Button>Create Group</Button>
          </Link>
        }
        title="Groups"
      />

      {groups.length > 0 ? (
        <div className="space-y-4">
          {groups.map((group: Group) => {
            if (!group.GroupName) {
              return null
            }
            return (
              <ListCard
                key={group.GroupName}
                params={{ groupName: group.GroupName }}
                subtitle={group.Arn ?? ""}
                title={group.GroupName}
                to="/iam/list-groups/$groupName"
              />
            )
          })}
        </div>
      ) : (
        <p className="text-muted-foreground">No IAM groups found.</p>
      )}
    </>
  )
}
