import type { PlacementGroup } from "@aws-sdk/client-ec2"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { StateBadge } from "@/components/state-badge"
import { Button } from "@/components/ui/button"
import { ec2PlacementGroupsQueryOptions } from "@/queries/ec2"

export const Route = createFileRoute(
  "/_auth/ec2/(placement-groups)/describe-placement-groups/",
)({
  loader: async ({ context }) => {
    await context.queryClient.ensureQueryData(ec2PlacementGroupsQueryOptions)
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

      {placementGroups.length > 0 ? (
        <div className="space-y-4">
          {placementGroups.map((pg: PlacementGroup) => {
            if (!pg.GroupId) {
              return null
            }
            return (
              <ListCard
                badge={<StateBadge state={pg.State} />}
                key={pg.GroupId}
                params={{ id: pg.GroupId }}
                subtitle={`${pg.Strategy} \u2022 ${pg.GroupId}`}
                title={pg.GroupName ?? pg.GroupId}
                to="/ec2/describe-placement-groups/$id"
              />
            )
          })}
        </div>
      ) : (
        <p className="text-muted-foreground">No placement groups found.</p>
      )}
    </>
  )
}
