import type { NatGateway } from "@aws-sdk/client-ec2"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { StateBadge } from "@/components/state-badge"
import { Button } from "@/components/ui/button"
import { getNameTag } from "@/lib/utils"
import { ec2NatGatewaysQueryOptions } from "@/queries/ec2"

export const Route = createFileRoute(
  "/_auth/ec2/(nat-gateways)/describe-nat-gateways/",
)({
  loader: async ({ context }) => {
    await context.queryClient.ensureQueryData(ec2NatGatewaysQueryOptions)
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

      {natGateways.length > 0 ? (
        <div className="space-y-4">
          {natGateways.map((nat: NatGateway) => {
            if (!nat.NatGatewayId) {
              return null
            }
            const name = getNameTag(nat.Tags)
            return (
              <ListCard
                badge={<StateBadge state={nat.State} />}
                key={nat.NatGatewayId}
                params={{ id: nat.NatGatewayId }}
                subtitle={nat.SubnetId ?? ""}
                title={
                  name ? `${nat.NatGatewayId} (${name})` : nat.NatGatewayId
                }
                to="/ec2/describe-nat-gateways/$id"
              />
            )
          })}
        </div>
      ) : (
        <p className="text-muted-foreground">No NAT Gateways found.</p>
      )}
    </>
  )
}
