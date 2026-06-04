import type { RouteTable } from "@aws-sdk/client-ec2"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { getNameTag } from "@/lib/utils"
import { ec2RouteTablesQueryOptions } from "@/queries/ec2"

export const Route = createFileRoute(
  "/_auth/ec2/(route-tables)/describe-route-tables/",
)({
  loader: async ({ context }) => {
    await context.queryClient.ensureQueryData(ec2RouteTablesQueryOptions)
  },
  head: () => ({
    meta: [
      {
        title: "Route Tables | EC2 | Mulga",
      },
    ],
  }),
  component: RouteTables,
})

function RouteTables() {
  const { data } = useSuspenseQuery(ec2RouteTablesQueryOptions)

  const routeTables = data.RouteTables ?? []

  return (
    <>
      <PageHeading
        actions={
          <Link to="/ec2/create-route-table">
            <Button>Create Route Table</Button>
          </Link>
        }
        title="Route Tables"
      />

      {routeTables.length > 0 ? (
        <div className="space-y-4">
          {routeTables.map((rtb: RouteTable) => {
            if (!rtb.RouteTableId) {
              return null
            }
            const name = getNameTag(rtb.Tags)
            const isMain = rtb.Associations?.some((a) => a.Main) ?? false
            const subnetCount =
              rtb.Associations?.filter((a) => a.SubnetId).length ?? 0
            return (
              <ListCard
                badge={isMain ? <Badge variant="secondary">Main</Badge> : null}
                key={rtb.RouteTableId}
                params={{ id: rtb.RouteTableId }}
                subtitle={`${rtb.VpcId ?? ""} · ${subnetCount} subnet${
                  subnetCount === 1 ? "" : "s"
                }`}
                title={
                  name ? `${rtb.RouteTableId} (${name})` : rtb.RouteTableId
                }
                to="/ec2/describe-route-tables/$id"
              />
            )
          })}
        </div>
      ) : (
        <p className="text-muted-foreground">No Route Tables found.</p>
      )}
    </>
  )
}
