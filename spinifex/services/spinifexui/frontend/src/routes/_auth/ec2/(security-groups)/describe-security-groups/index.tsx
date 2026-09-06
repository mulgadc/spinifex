import type { SecurityGroup } from "@aws-sdk/client-ec2"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { Button } from "@/components/ui/button"
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

      {securityGroups.length > 0 ? (
        <div className="space-y-4">
          {securityGroups.map((sg: SecurityGroup) => {
            if (!sg.GroupId) {
              return null
            }
            return (
              <ListCard
                key={sg.GroupId}
                params={{ id: sg.GroupId }}
                subtitle={sg.VpcId ?? ""}
                title={
                  sg.GroupName ? `${sg.GroupId} (${sg.GroupName})` : sg.GroupId
                }
                to="/ec2/describe-security-groups/$id"
              />
            )
          })}
        </div>
      ) : (
        <p className="text-muted-foreground">No security groups found.</p>
      )}
    </>
  )
}
