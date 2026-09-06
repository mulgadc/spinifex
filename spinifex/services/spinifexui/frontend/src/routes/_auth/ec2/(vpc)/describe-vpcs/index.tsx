import type { Vpc } from "@aws-sdk/client-ec2"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { StateBadge } from "@/components/state-badge"
import { Button } from "@/components/ui/button"
import { getNameTag } from "@/lib/utils"
import { ec2VpcsQueryOptions } from "@/queries/ec2"

export const Route = createFileRoute("/_auth/ec2/(vpc)/describe-vpcs/")({
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...ec2VpcsQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "VPCs | EC2 | Mulga",
      },
    ],
  }),
  component: Vpcs,
})

function Vpcs() {
  const { data } = useSuspenseQuery(ec2VpcsQueryOptions)

  const vpcs = data.Vpcs ?? []

  return (
    <>
      <PageHeading
        actions={
          <Link to="/ec2/create-vpc">
            <Button>Create VPC</Button>
          </Link>
        }
        title="VPCs"
      />

      {vpcs.length > 0 ? (
        <div className="space-y-4">
          {vpcs.map((vpc: Vpc) => {
            if (!vpc.VpcId) {
              return null
            }
            const name = getNameTag(vpc.Tags)
            return (
              <ListCard
                badge={<StateBadge state={vpc.State} />}
                key={vpc.VpcId}
                params={{ id: vpc.VpcId }}
                subtitle={vpc.CidrBlock ?? ""}
                title={name ? `${vpc.VpcId} (${name})` : vpc.VpcId}
                to="/ec2/describe-vpcs/$id"
              />
            )
          })}
        </div>
      ) : (
        <p className="text-muted-foreground">No VPCs found.</p>
      )}
    </>
  )
}
