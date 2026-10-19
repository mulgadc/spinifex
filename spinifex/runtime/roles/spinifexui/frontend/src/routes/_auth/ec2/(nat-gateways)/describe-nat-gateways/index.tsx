import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { ResourceList } from "@/components/resource-list"
import { StateBadge } from "@/components/state-badge"
import { Button } from "@/components/ui/button"
import { getNameTag } from "@/lib/utils"
import { ec2NatGatewaysQueryOptions } from "@/queries/ec2"

export const Route = createFileRoute(
  "/_auth/ec2/(nat-gateways)/describe-nat-gateways/",
)({
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...ec2NatGatewaysQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "NAT Gateways | EC2 | Mulga",
      },
    ],
  }),
  component: NatGateways,
})

function NatGateways() {
  const { data } = useSuspenseQuery(ec2NatGatewaysQueryOptions)

  const natGateways = data.NatGateways ?? []

  return (
    <>
      <PageHeading
        actions={
          <Link to="/ec2/create-nat-gateway">
            <Button>Create NAT Gateway</Button>
          </Link>
        }
        title="NAT Gateways"
      />

      <ResourceList
        empty="No NAT Gateways found."
        getKey={(nat) => nat.NatGatewayId}
        items={natGateways}
        renderItem={(nat, id) => {
          const name = getNameTag(nat.Tags)

          return (
            <ListCard
              badge={<StateBadge state={nat.State} />}
              params={{ id }}
              subtitle={nat.SubnetId ?? ""}
              title={name ? `${id} (${name})` : id}
              to="/ec2/describe-nat-gateways/$id"
            />
          )
        }}
      />
    </>
  )
}
