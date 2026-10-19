import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { ResourceList } from "@/components/resource-list"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { ec2LaunchTemplatesQueryOptions } from "@/queries/ec2"

export const Route = createFileRoute(
  "/_auth/ec2/(launch-templates)/describe-launch-templates/",
)({
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...ec2LaunchTemplatesQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "Launch Templates | EC2 | Mulga",
      },
    ],
  }),
  component: LaunchTemplates,
})

function LaunchTemplates() {
  const { data } = useSuspenseQuery(ec2LaunchTemplatesQueryOptions)

  const launchTemplates = data.LaunchTemplates ?? []

  return (
    <>
      <PageHeading
        actions={
          <Link to="/ec2/create-launch-template">
            <Button>Create Launch Template</Button>
          </Link>
        }
        title="Launch Templates"
      />

      <ResourceList
        empty="No launch templates found."
        getKey={(lt) => lt.LaunchTemplateId}
        items={launchTemplates}
        renderItem={(lt, id) => (
          <ListCard
            badge={
              <Badge variant="secondary">
                default v{lt.DefaultVersionNumber}
              </Badge>
            }
            params={{ id }}
            subtitle={`${id} • latest v${lt.LatestVersionNumber}`}
            title={lt.LaunchTemplateName ?? id}
            to="/ec2/describe-launch-templates/$id"
          />
        )}
      />
    </>
  )
}
