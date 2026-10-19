import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { ResourceList } from "@/components/resource-list"
import { Button } from "@/components/ui/button"
import { byName } from "@/lib/utils"
import { iamGroupsQueryOptions } from "@/queries/iam"

export const Route = createFileRoute("/_auth/iam/(groups)/list-groups/")({
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...iamGroupsQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [{ title: "Groups | IAM | Mulga" }],
  }),
  component: Groups,
})

function Groups() {
  const { data } = useSuspenseQuery(iamGroupsQueryOptions)

  const groups = (data.Groups ?? []).toSorted(byName((g) => g.GroupName))

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

      <ResourceList
        empty="No IAM groups found."
        getKey={(group) => group.GroupName}
        items={groups}
        renderItem={(group, groupName) => (
          <ListCard
            params={{ groupName }}
            subtitle={group.Arn ?? ""}
            title={groupName}
            to="/iam/list-groups/$groupName"
          />
        )}
      />
    </>
  )
}
