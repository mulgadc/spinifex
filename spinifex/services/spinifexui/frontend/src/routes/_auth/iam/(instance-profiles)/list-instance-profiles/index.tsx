import type { InstanceProfile } from "@aws-sdk/client-iam"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { Button } from "@/components/ui/button"
import { iamInstanceProfilesQueryOptions } from "@/queries/iam"

export const Route = createFileRoute(
  "/_auth/iam/(instance-profiles)/list-instance-profiles/",
)({
  loader: async ({ context }) => {
    await context.queryClient.ensureQueryData(iamInstanceProfilesQueryOptions)
  },
  head: () => ({
    meta: [{ title: "Instance Profiles | IAM | Mulga" }],
  }),
  component: InstanceProfiles,
})

function InstanceProfiles() {
  const { data } = useSuspenseQuery(iamInstanceProfilesQueryOptions)

  const profiles = (data.InstanceProfiles ?? []).toSorted((a, b) => {
    const nameA = a.InstanceProfileName?.toLowerCase() ?? ""
    const nameB = b.InstanceProfileName?.toLowerCase() ?? ""
    return nameA.localeCompare(nameB)
  })

  return (
    <>
      <PageHeading
        actions={
          <Link to="/iam/create-instance-profile">
            <Button>Create Instance Profile</Button>
          </Link>
        }
        title="Instance Profiles"
      />

      {profiles.length > 0 ? (
        <div className="space-y-4">
          {profiles.map((profile: InstanceProfile) => {
            if (!profile.InstanceProfileName) {
              return null
            }
            return (
              <ListCard
                key={profile.InstanceProfileName}
                params={{ instanceProfileName: profile.InstanceProfileName }}
                subtitle={profile.Arn ?? ""}
                title={profile.InstanceProfileName}
                to="/iam/list-instance-profiles/$instanceProfileName"
              />
            )
          })}
        </div>
      ) : (
        <p className="text-muted-foreground">No instance profiles found.</p>
      )}
    </>
  )
}
