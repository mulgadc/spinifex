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
import { useDeleteNatGateway } from "@/mutations/ec2"
import { ec2NatGatewayQueryOptions } from "@/queries/ec2"

export const Route = createFileRoute(
  "/_auth/ec2/(nat-gateways)/describe-nat-gateways/$id",
)({
  loader: async ({ context, params }) => {
    await context.queryClient.ensureQueryData(
      ec2NatGatewayQueryOptions(params.id),
    )
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `${params.id} | NAT Gateway | Mulga`,
      },
    ],
  }),
  component: NatGatewayDetail,
})

function NatGatewayDetail() {
  const { id } = Route.useParams()
  const navigate = useNavigate()
  const { data } = useSuspenseQuery(ec2NatGatewayQueryOptions(id))
  const deleteMutation = useDeleteNatGateway()
  const [showDeleteDialog, setShowDeleteDialog] = useState(false)

  const nat = data.NatGateways?.[0]

  const handleDelete = async () => {
    try {
      await deleteMutation.mutateAsync(id)
      navigate({ to: "/ec2/describe-nat-gateways" })
    } finally {
      setShowDeleteDialog(false)
    }
  }

  if (!nat?.NatGatewayId) {
    return (
      <>
        <BackLink to="/ec2/describe-nat-gateways">
          Back to NAT Gateways
        </BackLink>
        <p className="text-muted-foreground">NAT Gateway not found.</p>
      </>
    )
  }

  const name = getNameTag(nat.Tags)
  const eip = nat.NatGatewayAddresses?.[0]

  return (
    <>
      <BackLink to="/ec2/describe-nat-gateways">Back to NAT Gateways</BackLink>

      {deleteMutation.error && (
        <ErrorBanner
          error={deleteMutation.error}
          msg="Failed to delete NAT Gateway"
        />
      )}

      <div className="space-y-6">
        <PageHeading
          actions={
            <div className="flex items-center gap-2">
              <Button
                disabled={deleteMutation.isPending}
                onClick={() => setShowDeleteDialog(true)}
                size="sm"
                variant="destructive"
              >
                <Trash2 className="size-4" />
                Delete
              </Button>
              <StateBadge state={nat.State} />
            </div>
          }
          subtitle="NAT Gateway Details"
          title={name ? `${nat.NatGatewayId} (${name})` : nat.NatGatewayId}
        />

        <DetailCard>
          <DetailCard.Header>NAT Gateway Information</DetailCard.Header>
          <DetailCard.Content>
            <DetailRow label="NAT Gateway ID" value={nat.NatGatewayId} />
            <DetailRow label="State" value={nat.State} />
            <DetailRow label="Connectivity" value={nat.ConnectivityType} />
            <DetailRow
              label="Subnet ID"
              value={
                nat.SubnetId ? (
                  <Link
                    className="text-primary hover:underline"
                    params={{ id: nat.SubnetId }}
                    to="/ec2/describe-subnets/$id"
                  >
                    {nat.SubnetId}
                  </Link>
                ) : (
                  "—"
                )
              }
            />
            <DetailRow
              label="VPC ID"
              value={
                nat.VpcId ? (
                  <Link
                    className="text-primary hover:underline"
                    params={{ id: nat.VpcId }}
                    to="/ec2/describe-vpcs/$id"
                  >
                    {nat.VpcId}
                  </Link>
                ) : (
                  "—"
                )
              }
            />
            <DetailRow label="Public IP" value={eip?.PublicIp ?? "—"} />
            <DetailRow
              label="Allocation ID"
              value={
                eip?.AllocationId ? (
                  <Link
                    className="text-primary hover:underline"
                    params={{ id: eip.AllocationId }}
                    to="/ec2/describe-addresses/$id"
                  >
                    {eip.AllocationId}
                  </Link>
                ) : (
                  "—"
                )
              }
            />
          </DetailCard.Content>
        </DetailCard>
      </div>

      <DeleteConfirmationDialog
        description={`Are you sure you want to delete the NAT Gateway "${nat.NatGatewayId}"? This action cannot be undone. The associated Elastic IP is not released.`}
        isPending={deleteMutation.isPending}
        onConfirm={handleDelete}
        onOpenChange={setShowDeleteDialog}
        open={showDeleteDialog}
        title="Delete NAT Gateway"
      />
    </>
  )
}
