import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { BackLink } from "@/components/back-link"
import { DeleteResourceButton } from "@/components/delete-resource-button"
import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import { PageHeading } from "@/components/page-heading"
import { StateBadge } from "@/components/state-badge"
import { getNameTag } from "@/lib/utils"
import { useDeleteSubnet } from "@/mutations/ec2"
import { ec2SubnetQueryOptions } from "@/queries/ec2"

export const Route = createFileRoute(
  "/_auth/ec2/(subnet)/describe-subnets/$id",
)({
  loader: async ({ context, params }) => {
    await context.queryClient.query({
      ...ec2SubnetQueryOptions(params.id),
      staleTime: "static",
    })
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
  const { data } = useSuspenseQuery(ec2SubnetQueryOptions(id))
  const subnet = data.Subnets?.[0]
  const deleteMutation = useDeleteSubnet()

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

      <div className="space-y-6">
        <PageHeading
          actions={
            <div className="flex items-center gap-2">
              <DeleteResourceButton
                description={`Are you sure you want to delete the subnet "${subnet.SubnetId}"? This action cannot be undone.`}
                id={id}
                mutation={deleteMutation}
                title="Delete Subnet"
                to="/ec2/describe-subnets"
              />
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
    </>
  )
}
