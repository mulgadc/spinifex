import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { ResourceList } from "@/components/resource-list"
import { Button } from "@/components/ui/button"
import { securityGroupLabel } from "@/lib/utils"
import { ec2SecurityGroupsQueryOptions } from "@/queries/ec2"

export const Route = createFileRoute(
  "/_auth/ec2/(security-groups)/describe-security-groups/",
)({
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...ec2SecurityGroupsQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "Security Groups | EC2 | Mulga",
      },
    ],
  }),
  component: SecurityGroups,
})

function SecurityGroups() {
  const { data } = useSuspenseQuery(ec2SecurityGroupsQueryOptions)

  const securityGroups = data.SecurityGroups ?? []

  return (
    <>
      <PageHeading
        actions={
          <Link to="/ec2/create-security-group">
            <Button>Create Security Group</Button>
          </Link>
        }
        title="Security Groups"
      />

      <ResourceList
        empty="No security groups found."
        getKey={(sg) => sg.GroupId}
        items={securityGroups}
        renderItem={(sg, id) => (
          <ListCard
            params={{ id }}
            subtitle={sg.VpcId ?? ""}
            title={securityGroupLabel(sg)}
            to="/ec2/describe-security-groups/$id"
          />
        )}
      />
    </>
  )
}
