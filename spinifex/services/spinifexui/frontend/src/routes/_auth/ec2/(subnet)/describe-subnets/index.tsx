import type { Subnet } from "@aws-sdk/client-ec2"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { StateBadge } from "@/components/state-badge"
import { Button } from "@/components/ui/button"
import { getNameTag } from "@/lib/utils"
import { ec2SubnetsQueryOptions } from "@/queries/ec2"

export const Route = createFileRoute("/_auth/ec2/(subnet)/describe-subnets/")({
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...ec2SubnetsQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "Subnets | EC2 | Mulga",
      },
    ],
  }),
  component: Subnets,
})

function Subnets() {
  const { data } = useSuspenseQuery(ec2SubnetsQueryOptions)

  const subnets = data.Subnets ?? []

  return (
    <>
      <PageHeading
        actions={
          <Link to="/ec2/create-subnet">
            <Button>Create Subnet</Button>
          </Link>
        }
        title="Subnets"
      />

      {subnets.length > 0 ? (
        <div className="space-y-4">
          {subnets.map((subnet: Subnet) => {
            if (!subnet.SubnetId) {
              return null
            }
            const name = getNameTag(subnet.Tags)
            return (
              <ListCard
                badge={<StateBadge state={subnet.State} />}
                key={subnet.SubnetId}
                params={{ id: subnet.SubnetId }}
                subtitle={`${subnet.CidrBlock ?? ""} \u2022 ${subnet.AvailabilityZone ?? ""}`}
                title={name ? `${subnet.SubnetId} (${name})` : subnet.SubnetId}
                to="/ec2/describe-subnets/$id"
              />
            )
          })}
        </div>
      ) : (
        <p className="text-muted-foreground">No subnets found.</p>
      )}
    </>
  )
}
