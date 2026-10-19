import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { BackLink } from "@/components/back-link"
import { DeleteResourceButton } from "@/components/delete-resource-button"
import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import { PageHeading } from "@/components/page-heading"
import { StateBadge } from "@/components/state-badge"
import { useDeletePlacementGroup } from "@/mutations/ec2"
import {
  ec2InstancesQueryOptions,
  ec2PlacementGroupQueryOptions,
} from "@/queries/ec2"

export const Route = createFileRoute(
  "/_auth/ec2/(placement-groups)/describe-placement-groups/$id",
)({
  loader: async ({ context, params }) => {
    await Promise.all([
      context.queryClient.query({
        ...ec2PlacementGroupQueryOptions(params.id),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...ec2InstancesQueryOptions,
        staleTime: "static",
      }),
    ])
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `${params.id} | EC2 | Mulga`,
      },
    ],
  }),
  component: PlacementGroupDetail,
})

function PlacementGroupDetail() {
  const { id } = Route.useParams()
  const { data } = useSuspenseQuery(ec2PlacementGroupQueryOptions(id))
  const { data: instancesData } = useSuspenseQuery(ec2InstancesQueryOptions)
  const deleteMutation = useDeletePlacementGroup()

  const pg = data.PlacementGroups?.[0]

  const instances =
    instancesData.Reservations?.flatMap((r) => r.Instances ?? []).filter(
      (inst) => inst.Placement?.GroupName === pg?.GroupName,
    ) ?? []

  if (!pg?.GroupId) {
    return (
      <>
        <BackLink to="/ec2/describe-placement-groups">
          Back to placement groups
        </BackLink>
        <p className="text-muted-foreground">Placement group not found.</p>
      </>
    )
  }

  return (
    <>
      <BackLink to="/ec2/describe-placement-groups">
        Back to placement groups
      </BackLink>

      <div className="space-y-6">
        <PageHeading
          actions={
            <div className="flex items-center gap-2">
              <DeleteResourceButton
                description={`Are you sure you want to delete the placement group "${pg.GroupName}"? The group must have no running instances.`}
                disabled={!pg.GroupName}
                id={pg.GroupName ?? ""}
                mutation={deleteMutation}
                title="Delete Placement Group"
                to="/ec2/describe-placement-groups"
              />
              <StateBadge state={pg.State} />
            </div>
          }
          subtitle="Placement Group Details"
          title={pg.GroupName ?? pg.GroupId}
        />

        <DetailCard>
          <DetailCard.Header>Placement Group Information</DetailCard.Header>
          <DetailCard.Content>
            <DetailRow label="Group ID" value={pg.GroupId} />
            <DetailRow label="Group Name" value={pg.GroupName} />
            <DetailRow label="Strategy" value={pg.Strategy} />
            <DetailRow label="State" value={pg.State} />
            <DetailRow label="Spread Level" value={pg.SpreadLevel} />
          </DetailCard.Content>
        </DetailCard>

        <DetailCard>
          <DetailCard.Header>Instances</DetailCard.Header>
          {instances.length > 0 ? (
            instances.map((inst) => (
              <DetailCard.Content key={inst.InstanceId}>
                <DetailRow
                  label="Instance ID"
                  value={
                    inst.InstanceId ? (
                      <Link
                        className="text-primary hover:underline"
                        params={{ id: inst.InstanceId }}
                        to="/ec2/describe-instances/$id"
                      >
                        {inst.InstanceId}
                      </Link>
                    ) : undefined
                  }
                />
                <DetailRow label="State" value={inst.State?.Name} />
                <DetailRow label="Instance Type" value={inst.InstanceType} />
                <DetailRow
                  label="Availability Zone"
                  value={inst.Placement?.AvailabilityZone}
                />
              </DetailCard.Content>
            ))
          ) : (
            <DetailCard.Content>
              <p className="text-muted-foreground">
                No instances in this placement group.
              </p>
            </DetailCard.Content>
          )}
        </DetailCard>
      </div>
    </>
  )
}
