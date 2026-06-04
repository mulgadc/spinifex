import type { InternetGateway } from "@aws-sdk/client-ec2"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { Button } from "@/components/ui/button"
import { getNameTag } from "@/lib/utils"
import { ec2InternetGatewaysQueryOptions } from "@/queries/ec2"

export const Route = createFileRoute(
  "/_auth/ec2/(internet-gateways)/describe-internet-gateways/",
)({
  loader: async ({ context }) => {
    await context.queryClient.ensureQueryData(ec2InternetGatewaysQueryOptions)
  },
  head: () => ({
    meta: [
      {
        title: "Internet Gateways | EC2 | Mulga",
      },
    ],
  }),
  component: InternetGateways,
})

function InternetGateways() {
  const { data } = useSuspenseQuery(ec2InternetGatewaysQueryOptions)

  const internetGateways = data.InternetGateways ?? []

  return (
    <>
      <PageHeading
        actions={
          <Link to="/ec2/create-internet-gateway">
            <Button>Create Internet Gateway</Button>
          </Link>
        }
        title="Internet Gateways"
      />

      {internetGateways.length > 0 ? (
        <div className="space-y-4">
          {internetGateways.map((igw: InternetGateway) => {
            if (!igw.InternetGatewayId) {
              return null
            }
            const name = getNameTag(igw.Tags)
            const attachedVpc = igw.Attachments?.[0]?.VpcId
            return (
              <ListCard
                key={igw.InternetGatewayId}
                params={{ id: igw.InternetGatewayId }}
                subtitle={
                  attachedVpc ? `attached to ${attachedVpc}` : "detached"
                }
                title={
                  name
                    ? `${igw.InternetGatewayId} (${name})`
                    : igw.InternetGatewayId
                }
                to="/ec2/describe-internet-gateways/$id"
              />
            )
          })}
        </div>
      ) : (
        <p className="text-muted-foreground">No Internet Gateways found.</p>
      )}
    </>
  )
}
