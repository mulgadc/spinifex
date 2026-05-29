import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link, useNavigate } from "@tanstack/react-router"
import { Trash2 } from "lucide-react"
import { useState } from "react"

import { BackLink } from "@/components/back-link"
import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import { ErrorBanner } from "@/components/error-banner"
import { PageHeading } from "@/components/page-heading"
import { StateBadge } from "@/components/state-badge"
import { Button } from "@/components/ui/button"
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
      context.queryClient.ensureQueryData(
        ec2PlacementGroupQueryOptions(params.id),
      ),
      context.queryClient.ensureQueryData(ec2InstancesQueryOptions),
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
  const navigate = useNavigate()
  const { data } = useSuspenseQuery(ec2PlacementGroupQueryOptions(id))
  const { data: instancesData } = useSuspenseQuery(ec2InstancesQueryOptions)
  const deleteMutation = useDeletePlacementGroup()
  const [showDeleteDialog, setShowDeleteDialog] = useState(false)

  const pg = data.PlacementGroups?.[0]

  const instances =
    instancesData.Reservations?.flatMap((r) => r.Instances ?? []).filter(
      (inst) => inst.Placement?.GroupName === pg?.GroupName,
    ) ?? []

  const handleDelete = async () => {
    if (!pg?.GroupName) {
      return
    }
    try {
      await deleteMutation.mutateAsync(pg.GroupName)
      navigate({ to: "/ec2/describe-placement-groups" })
    } finally {
      setShowDeleteDialog(false)
    }
  }

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

      {deleteMutation.error && (
        <ErrorBanner
          error={deleteMutation.error}
          msg="Failed to delete placement group"
        />
      )}

      <div className="space-y-6">
        <PageHeading
          actions={
            <div className="flex items-center gap-2">
              <Button
                onClick={() => setShowDeleteDialog(true)}
                size="sm"
                variant="destructive"
              >
                <Trash2 className="size-4" />
                Delete
              </Button>
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

      <DeleteConfirmationDialog
        description={`Are you sure you want to delete the placement group "${pg.GroupName}"? The group must have no running instances.`}
        isPending={deleteMutation.isPending}
        onConfirm={handleDelete}
        onOpenChange={setShowDeleteDialog}
        open={showDeleteDialog}
        title="Delete Placement Group"
      />
    </>
  )
}
