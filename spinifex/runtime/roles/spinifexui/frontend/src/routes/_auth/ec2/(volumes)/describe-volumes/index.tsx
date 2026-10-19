import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { ResourceList } from "@/components/resource-list"
import { StateBadge } from "@/components/state-badge"
import { Button } from "@/components/ui/button"
import { ec2VolumesQueryOptions } from "@/queries/ec2"

export const Route = createFileRoute("/_auth/ec2/(volumes)/describe-volumes/")({
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...ec2VolumesQueryOptions,
      staleTime: "static",
    })
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

      <ResourceList
        empty="No volumes found."
        getKey={(volume) => volume.VolumeId}
        items={volumes}
        renderItem={(volume, id) => (
          <ListCard
            badge={<StateBadge state={volume.State} />}
            params={{ id }}
            subtitle={`${volume.Size} GiB`}
            title={id}
            to="/ec2/describe-volumes/$id"
          />
        )}
      />
    </>
  )
}
