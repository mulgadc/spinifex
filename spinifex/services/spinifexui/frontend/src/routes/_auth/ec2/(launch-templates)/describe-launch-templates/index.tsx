import type { LaunchTemplate } from "@aws-sdk/client-ec2"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { ec2LaunchTemplatesQueryOptions } from "@/queries/ec2"

export const Route = createFileRoute(
  "/_auth/ec2/(launch-templates)/describe-launch-templates/",
)({
  loader: async ({ context }) => {
    await context.queryClient.ensureQueryData(ec2LaunchTemplatesQueryOptions)
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

      {launchTemplates.length > 0 ? (
        <div className="space-y-4">
          {launchTemplates.map((lt: LaunchTemplate) => {
            if (!lt.LaunchTemplateId) {
              return null
            }
            return (
              <ListCard
                badge={
                  <Badge variant="secondary">
                    default v{lt.DefaultVersionNumber}
                  </Badge>
                }
                key={lt.LaunchTemplateId}
                params={{ id: lt.LaunchTemplateId }}
                subtitle={`${lt.LaunchTemplateId} • latest v${lt.LatestVersionNumber}`}
                title={lt.LaunchTemplateName ?? lt.LaunchTemplateId}
                to="/ec2/describe-launch-templates/$id"
              />
            )
          })}
        </div>
      ) : (
        <p className="text-muted-foreground">No launch templates found.</p>
      )}
    </>
  )
}
