import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { ResourceList } from "@/components/resource-list"
import { Button } from "@/components/ui/button"
import { byName } from "@/lib/utils"
import { iamInstanceProfilesQueryOptions } from "@/queries/iam"

export const Route = createFileRoute(
  "/_auth/iam/(instance-profiles)/list-instance-profiles/",
)({
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...iamInstanceProfilesQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [{ title: "Instance Profiles | IAM | Mulga" }],
  }),
  component: InstanceProfiles,
})

function InstanceProfiles() {
  const { data } = useSuspenseQuery(iamInstanceProfilesQueryOptions)

  const profiles = (data.InstanceProfiles ?? []).toSorted(
    byName((p) => p.InstanceProfileName),
  )

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

      <ResourceList
        empty="No instance profiles found."
        getKey={(profile) => profile.InstanceProfileName}
        items={profiles}
        renderItem={(profile, instanceProfileName) => (
          <ListCard
            params={{ instanceProfileName }}
            subtitle={profile.Arn ?? ""}
            title={instanceProfileName}
            to="/iam/list-instance-profiles/$instanceProfileName"
          />
        )}
      />
    </>
  )
}
