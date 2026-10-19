import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { ResourceList } from "@/components/resource-list"
import { StateBadge } from "@/components/state-badge"
import { Button } from "@/components/ui/button"
import { ec2SnapshotsQueryOptions } from "@/queries/ec2"

export const Route = createFileRoute(
  "/_auth/ec2/(snapshots)/describe-snapshots/",
)({
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...ec2SnapshotsQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "Snapshots | EC2 | Mulga",
      },
    ],
  }),
  component: Snapshots,
})

function Snapshots() {
  const { data } = useSuspenseQuery(ec2SnapshotsQueryOptions)

  const snapshots = data.Snapshots ?? []

  return (
    <>
      <PageHeading
        actions={
          <Link to="/ec2/create-snapshot">
            <Button>Create Snapshot</Button>
          </Link>
        }
        title="Snapshots"
      />

      <ResourceList
        empty="No snapshots found."
        getKey={(snapshot) => snapshot.SnapshotId}
        items={snapshots}
        renderItem={(snapshot, id) => (
          <ListCard
            badge={<StateBadge state={snapshot.State} />}
            params={{ id }}
            subtitle={`${snapshot.VolumeSize} GiB \u2022 ${snapshot.VolumeId}`}
            title={id}
            to="/ec2/describe-snapshots/$id"
          />
        )}
      />
    </>
  )
}
