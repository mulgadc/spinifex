import type { Snapshot } from "@aws-sdk/client-ec2"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { StateBadge } from "@/components/state-badge"
import { Button } from "@/components/ui/button"
import { ec2SnapshotsQueryOptions } from "@/queries/ec2"

export const Route = createFileRoute(
  "/_auth/ec2/(snapshots)/describe-snapshots/",
)({
  loader: async ({ context }) => {
    await context.queryClient.ensureQueryData(ec2SnapshotsQueryOptions)
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

      {snapshots.length > 0 ? (
        <div className="space-y-4">
          {snapshots.map((snapshot: Snapshot) => {
            if (!snapshot.SnapshotId) {
              return null
            }
            return (
              <ListCard
                badge={<StateBadge state={snapshot.State} />}
                key={snapshot.SnapshotId}
                params={{ id: snapshot.SnapshotId }}
                subtitle={`${snapshot.VolumeSize} GiB \u2022 ${snapshot.VolumeId}`}
                title={snapshot.SnapshotId}
                to="/ec2/describe-snapshots/$id"
              />
            )
          })}
        </div>
      ) : (
        <p className="text-muted-foreground">No snapshots found.</p>
      )}
    </>
  )
}
