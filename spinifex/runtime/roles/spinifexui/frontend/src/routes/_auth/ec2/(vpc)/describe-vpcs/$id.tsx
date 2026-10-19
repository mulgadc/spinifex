import type { Subnet } from "@aws-sdk/client-ec2"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { BackLink } from "@/components/back-link"
import { DeleteResourceButton } from "@/components/delete-resource-button"
import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import { PageHeading } from "@/components/page-heading"
import { StateBadge } from "@/components/state-badge"
import { getNameTag } from "@/lib/utils"
import { useDeleteVpc } from "@/mutations/ec2"
import { ec2SubnetsQueryOptions, ec2VpcQueryOptions } from "@/queries/ec2"

export const Route = createFileRoute("/_auth/ec2/(vpc)/describe-vpcs/$id")({
  loader: async ({ context, params }) => {
    await Promise.all([
      context.queryClient.query({
        ...ec2VpcQueryOptions(params.id),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...ec2SubnetsQueryOptions,
        staleTime: "static",
      }),
    ])
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `${params.id} | VPC | Mulga`,
      },
    ],
  }),
  component: VpcDetail,
})

function VpcDetail() {
  const { id } = Route.useParams()
  const { data } = useSuspenseQuery(ec2VpcQueryOptions(id))
  const { data: subnetsData } = useSuspenseQuery(ec2SubnetsQueryOptions)
  const vpc = data.Vpcs?.[0]
  const deleteMutation = useDeleteVpc()

  const associatedSubnets =
    subnetsData.Subnets?.filter((s: Subnet) => s.VpcId === id) ?? []

  if (!vpc?.VpcId) {
    return (
      <>
        <BackLink to="/ec2/describe-vpcs">Back to VPCs</BackLink>
        <p className="text-muted-foreground">VPC not found.</p>
      </>
    )
  }

  const name = getNameTag(vpc.Tags)

  return (
    <>
      <BackLink to="/ec2/describe-vpcs">Back to VPCs</BackLink>

      <div className="space-y-6">
        <PageHeading
          actions={
            <div className="flex items-center gap-2">
              <DeleteResourceButton
                description={`Are you sure you want to delete the VPC "${vpc.VpcId}"? This action cannot be undone. The VPC must have no associated subnets or internet gateways.`}
                id={id}
                mutation={deleteMutation}
                title="Delete VPC"
                to="/ec2/describe-vpcs"
              />
              <StateBadge state={vpc.State} />
            </div>
          }
          subtitle="VPC Details"
          title={name ? `${vpc.VpcId} (${name})` : vpc.VpcId}
        />

        <DetailCard>
          <DetailCard.Header>VPC Information</DetailCard.Header>
          <DetailCard.Content>
            <DetailRow label="VPC ID" value={vpc.VpcId} />
            <DetailRow label="State" value={vpc.State} />
            <DetailRow label="CIDR Block" value={vpc.CidrBlock} />
            <DetailRow
              label="Is Default"
              value={vpc.IsDefault ? "Yes" : "No"}
            />
            <DetailRow label="DHCP Options ID" value={vpc.DhcpOptionsId} />
            <DetailRow label="Instance Tenancy" value={vpc.InstanceTenancy} />
            <DetailRow label="Owner ID" value={vpc.OwnerId} />
          </DetailCard.Content>
        </DetailCard>

        {associatedSubnets.length > 0 && (
          <DetailCard>
            <DetailCard.Header>
              Subnets ({associatedSubnets.length})
            </DetailCard.Header>
            {associatedSubnets.map((subnet: Subnet) => {
              if (!subnet.SubnetId) {
                return null
              }

              return (
                <DetailCard.Content key={subnet.SubnetId}>
                  <DetailRow
                    label="Subnet ID"
                    value={
                      <Link
                        className="text-primary hover:underline"
                        params={{ id: subnet.SubnetId }}
                        to="/ec2/describe-subnets/$id"
                      >
                        {subnet.SubnetId}
                      </Link>
                    }
                  />
                  <DetailRow label="CIDR Block" value={subnet.CidrBlock} />
                  <DetailRow
                    label="Availability Zone"
                    value={subnet.AvailabilityZone}
                  />
                  <DetailRow label="State" value={subnet.State} />
                </DetailCard.Content>
              )
            })}
          </DetailCard>
        )}
      </div>
    </>
  )
}
