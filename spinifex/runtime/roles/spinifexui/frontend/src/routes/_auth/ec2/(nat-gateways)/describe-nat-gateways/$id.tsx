import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { BackLink } from "@/components/back-link"
import { DeleteResourceButton } from "@/components/delete-resource-button"
import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import { PageHeading } from "@/components/page-heading"
import { StateBadge } from "@/components/state-badge"
import { getNameTag } from "@/lib/utils"
import { useDeleteNatGateway } from "@/mutations/ec2"
import { ec2NatGatewayQueryOptions } from "@/queries/ec2"

export const Route = createFileRoute(
  "/_auth/ec2/(nat-gateways)/describe-nat-gateways/$id",
)({
  loader: async ({ context, params }) => {
    await context.queryClient.query({
      ...ec2NatGatewayQueryOptions(params.id),
      staleTime: "static",
    })
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
  const { data } = useSuspenseQuery(ec2NatGatewayQueryOptions(id))
  const deleteMutation = useDeleteNatGateway()

  const nat = data.NatGateways?.[0]

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

      <div className="space-y-6">
        <PageHeading
          actions={
            <div className="flex items-center gap-2">
              <DeleteResourceButton
                description={`Are you sure you want to delete the NAT Gateway "${nat.NatGatewayId}"? This action cannot be undone. The associated Elastic IP is not released.`}
                id={id}
                mutation={deleteMutation}
                title="Delete NAT Gateway"
                to="/ec2/describe-nat-gateways"
              />
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
    </>
  )
}
