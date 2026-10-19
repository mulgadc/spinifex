import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { ResourceList } from "@/components/resource-list"
import { Button } from "@/components/ui/button"
import { getNameTag } from "@/lib/utils"
import { ec2InternetGatewaysQueryOptions } from "@/queries/ec2"

export const Route = createFileRoute(
  "/_auth/ec2/(internet-gateways)/describe-internet-gateways/",
)({
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...ec2InternetGatewaysQueryOptions,
      staleTime: "static",
    })
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

      <ResourceList
        empty="No Internet Gateways found."
        getKey={(igw) => igw.InternetGatewayId}
        items={internetGateways}
        renderItem={(igw, id) => {
          const name = getNameTag(igw.Tags)
          const attachedVpc = igw.Attachments?.[0]?.VpcId

          return (
            <ListCard
              params={{ id }}
              subtitle={attachedVpc ? `attached to ${attachedVpc}` : "detached"}
              title={name ? `${id} (${name})` : id}
              to="/ec2/describe-internet-gateways/$id"
            />
          )
        }}
      />
    </>
  )
}
