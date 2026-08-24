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
import { getNameTag } from "@/lib/utils"
import { useDeleteSubnet } from "@/mutations/ec2"
import { ec2SubnetQueryOptions } from "@/queries/ec2"

export const Route = createFileRoute(
  "/_auth/ec2/(subnet)/describe-subnets/$id",
)({
  loader: async ({ context, params }) => {
    await context.queryClient.ensureQueryData(ec2SubnetQueryOptions(params.id))
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `${params.id} | Subnet | Mulga`,
      },
    ],
  }),
  component: SubnetDetail,
})

function SubnetDetail() {
  const { id } = Route.useParams()
  const navigate = useNavigate()
  const { data } = useSuspenseQuery(ec2SubnetQueryOptions(id))
  const subnet = data.Subnets?.[0]
  const deleteMutation = useDeleteSubnet()
  const [showDeleteDialog, setShowDeleteDialog] = useState(false)

  const handleDelete = async () => {
    try {
      await deleteMutation.mutateAsync(id)
      navigate({ to: "/ec2/describe-subnets" })
    } finally {
      setShowDeleteDialog(false)
    }
  }

  if (!subnet?.SubnetId) {
    return (
      <>
        <BackLink to="/ec2/describe-subnets">Back to Subnets</BackLink>
        <p className="text-muted-foreground">Subnet not found.</p>
      </>
    )
  }

  const name = getNameTag(subnet.Tags)

  return (
    <>
      <BackLink to="/ec2/describe-subnets">Back to Subnets</BackLink>

      {deleteMutation.error && (
        <ErrorBanner
          error={deleteMutation.error}
          msg="Failed to delete subnet"
        />
      )}

      <div className="space-y-6">
        <PageHeading
          actions={
            <div className="flex items-center gap-2">
              <Button
                onClick={() => {
                  setShowDeleteDialog(true)
                }}
                size="sm"
                variant="destructive"
              >
                <Trash2 className="size-4" />
                Delete
              </Button>
              <StateBadge state={subnet.State} />
            </div>
          }
          subtitle="Subnet Details"
          title={name ? `${subnet.SubnetId} (${name})` : subnet.SubnetId}
        />

        <DetailCard>
          <DetailCard.Header>Subnet Information</DetailCard.Header>
          <DetailCard.Content>
            <DetailRow label="Subnet ID" value={subnet.SubnetId} />
            <DetailRow
              label="VPC ID"
              value={
                subnet.VpcId ? (
                  <Link
                    className="text-primary hover:underline"
                    params={{ id: subnet.VpcId }}
                    to="/ec2/describe-vpcs/$id"
                  >
                    {subnet.VpcId}
                  </Link>
                ) : (
                  "\u2014"
                )
              }
            />
            <DetailRow label="State" value={subnet.State} />
            <DetailRow label="CIDR Block" value={subnet.CidrBlock} />
            <DetailRow
              label="Availability Zone"
              value={subnet.AvailabilityZone}
            />
            <DetailRow
              label="Available IP Count"
              value={subnet.AvailableIpAddressCount?.toString()}
            />
            <DetailRow
              label="Map Public IP on Launch"
              value={subnet.MapPublicIpOnLaunch ? "Yes" : "No"}
            />
            <DetailRow
              label="Default for AZ"
              value={subnet.DefaultForAz ? "Yes" : "No"}
            />
          </DetailCard.Content>
        </DetailCard>
      </div>

      <DeleteConfirmationDialog
        description={`Are you sure you want to delete the subnet "${subnet.SubnetId}"? This action cannot be undone.`}
        isPending={deleteMutation.isPending}
        onConfirm={handleDelete}
        onOpenChange={setShowDeleteDialog}
        open={showDeleteDialog}
        title="Delete Subnet"
      />
    </>
  )
}
