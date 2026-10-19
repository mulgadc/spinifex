import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { ResourceList } from "@/components/resource-list"
import { StateBadge } from "@/components/state-badge"
import { Button } from "@/components/ui/button"
import { ec2PlacementGroupsQueryOptions } from "@/queries/ec2"

export const Route = createFileRoute(
  "/_auth/ec2/(placement-groups)/describe-placement-groups/",
)({
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...ec2PlacementGroupsQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "Placement Groups | EC2 | Mulga",
      },
    ],
  }),
  component: PlacementGroups,
})

function PlacementGroups() {
  const { data } = useSuspenseQuery(ec2PlacementGroupsQueryOptions)

  const placementGroups = data.PlacementGroups ?? []

  return (
    <>
      <PageHeading
        actions={
          <Link to="/ec2/create-placement-group">
            <Button>Create Placement Group</Button>
          </Link>
        }
        title="Placement Groups"
      />

      <ResourceList
        empty="No placement groups found."
        getKey={(pg) => pg.GroupId}
        items={placementGroups}
        renderItem={(pg, id) => (
          <ListCard
            badge={<StateBadge state={pg.State} />}
            params={{ id }}
            subtitle={`${pg.Strategy} \u2022 ${id}`}
            title={pg.GroupName ?? id}
            to="/ec2/describe-placement-groups/$id"
          />
        )}
      />
    </>
  )
}
