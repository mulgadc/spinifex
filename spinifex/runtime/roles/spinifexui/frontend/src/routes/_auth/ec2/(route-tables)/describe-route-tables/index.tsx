import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { ResourceList } from "@/components/resource-list"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { getNameTag } from "@/lib/utils"
import { ec2RouteTablesQueryOptions } from "@/queries/ec2"

export const Route = createFileRoute(
  "/_auth/ec2/(route-tables)/describe-route-tables/",
)({
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...ec2RouteTablesQueryOptions,
      staleTime: "static",
    })
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

      <ResourceList
        empty="No Route Tables found."
        getKey={(rtb) => rtb.RouteTableId}
        items={routeTables}
        renderItem={(rtb, id) => {
          const name = getNameTag(rtb.Tags)
          const isMain = rtb.Associations?.some((a) => a.Main) ?? false

          const subnetCount =
            rtb.Associations?.filter((a) => a.SubnetId).length ?? 0

          return (
            <ListCard
              badge={isMain ? <Badge variant="secondary">Main</Badge> : null}
              params={{ id }}
              subtitle={`${rtb.VpcId ?? ""} · ${subnetCount} subnet${
                subnetCount === 1 ? "" : "s"
              }`}
              title={name ? `${id} (${name})` : id}
              to="/ec2/describe-route-tables/$id"
            />
          )
        }}
      />
    </>
  )
}
