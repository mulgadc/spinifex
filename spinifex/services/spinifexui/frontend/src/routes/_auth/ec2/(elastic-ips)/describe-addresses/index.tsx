import type { Address } from "@aws-sdk/client-ec2"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { Button } from "@/components/ui/button"
import { getNameTag } from "@/lib/utils"
import { ec2AddressesQueryOptions } from "@/queries/ec2"

export const Route = createFileRoute(
  "/_auth/ec2/(elastic-ips)/describe-addresses/",
)({
  loader: async ({ context }) => {
    await context.queryClient.ensureQueryData(ec2AddressesQueryOptions)
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

      {addresses.length > 0 ? (
        <div className="space-y-4">
          {addresses.map((addr: Address) => {
            if (!addr.AllocationId) {
              return null
            }
            const name = getNameTag(addr.Tags)
            const subtitle = addr.AssociationId
              ? `${addr.PublicIp} • associated`
              : `${addr.PublicIp} • available`
            return (
              <ListCard
                key={addr.AllocationId}
                params={{ id: addr.AllocationId }}
                subtitle={subtitle}
                title={
                  name
                    ? `${addr.AllocationId} (${name})`
                    : (addr.AllocationId ?? "")
                }
                to="/ec2/describe-addresses/$id"
              />
            )
          })}
        </div>
      ) : (
        <p className="text-muted-foreground">No Elastic IPs found.</p>
      )}
    </>
  )
}
