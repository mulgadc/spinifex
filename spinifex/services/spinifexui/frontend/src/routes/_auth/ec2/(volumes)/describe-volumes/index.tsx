import type { Volume } from "@aws-sdk/client-ec2"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { StateBadge } from "@/components/state-badge"
import { Button } from "@/components/ui/button"
import { ec2VolumesQueryOptions } from "@/queries/ec2"

export const Route = createFileRoute("/_auth/ec2/(volumes)/describe-volumes/")({
  loader: async ({ context }) => {
    await context.queryClient.ensureQueryData(ec2VolumesQueryOptions)
  },
  head: () => ({
    meta: [
      {
        title: "Volumes | EC2 | Mulga",
      },
    ],
  }),
  component: Volumes,
})

function Volumes() {
  const { data } = useSuspenseQuery(ec2VolumesQueryOptions)

  const volumes = data.Volumes ?? []

  return (
    <>
      <PageHeading
        actions={
          <Link to="/ec2/create-volume">
            <Button>Create Volume</Button>
          </Link>
        }
        title="Volumes"
      />

      {volumes.length > 0 ? (
        <div className="space-y-4">
          {volumes.map((volume: Volume) => {
            if (!volume.VolumeId) {
              return null
            }
            return (
              <ListCard
                badge={<StateBadge state={volume.State} />}
                key={volume.VolumeId}
                params={{ id: volume.VolumeId }}
                subtitle={`${volume.Size} GiB`}
                title={volume.VolumeId}
                to="/ec2/describe-volumes/$id"
              />
            )
          })}
        </div>
      ) : (
        <p className="text-muted-foreground">No volumes found.</p>
      )}
    </>
  )
}
