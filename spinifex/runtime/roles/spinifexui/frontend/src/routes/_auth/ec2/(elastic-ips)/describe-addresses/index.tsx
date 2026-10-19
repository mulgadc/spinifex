import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { ResourceList } from "@/components/resource-list"
import { Button } from "@/components/ui/button"
import { getNameTag } from "@/lib/utils"
import { ec2AddressesQueryOptions } from "@/queries/ec2"

export const Route = createFileRoute(
  "/_auth/ec2/(elastic-ips)/describe-addresses/",
)({
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...ec2AddressesQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "Elastic IPs | EC2 | Mulga",
      },
    ],
  }),
  component: Addresses,
})

function Addresses() {
  const { data } = useSuspenseQuery(ec2AddressesQueryOptions)

  const addresses = data.Addresses ?? []

  return (
    <>
      <PageHeading
        actions={
          <Link to="/ec2/allocate-address">
            <Button>Allocate Elastic IP</Button>
          </Link>
        }
        title="Elastic IPs"
      />

      <ResourceList
        empty="No Elastic IPs found."
        getKey={(addr) => addr.AllocationId}
        items={addresses}
        renderItem={(addr, id) => {
          const name = getNameTag(addr.Tags)

          const subtitle = addr.AssociationId
            ? `${addr.PublicIp} • associated`
            : `${addr.PublicIp} • available`

          return (
            <ListCard
              params={{ id }}
              subtitle={subtitle}
              title={name ? `${id} (${name})` : id}
              to="/ec2/describe-addresses/$id"
            />
          )
        }}
      />
    </>
  )
}
