import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute } from "@tanstack/react-router"

import { ListCard } from "@/components/list-card"
import { PageHeading } from "@/components/page-heading"
import { ResourceList } from "@/components/resource-list"
import { StateBadge } from "@/components/state-badge"
import { isSystemManagedImage } from "@/lib/system-managed"
import { ec2ImagesQueryOptions } from "@/queries/ec2"

export const Route = createFileRoute("/_auth/ec2/(images)/describe-images/")({
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...ec2ImagesQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "Images | EC2 | Mulga",
      },
    ],
  }),
  component: Images,
})

function Images() {
  const { data } = useSuspenseQuery(ec2ImagesQueryOptions)

  const images = (data.Images ?? []).filter(
    (image) => !isSystemManagedImage(image),
  )

  return (
    <>
      <PageHeading title="Images" />

      <ResourceList
        empty="No images found."
        getKey={(image) => image.ImageId}
        items={images}
        renderItem={(image, id) => (
          <ListCard
            badge={<StateBadge state={image.State} />}
            params={{ id }}
            subtitle={id}
            title={image.Name ?? id}
            to="/ec2/describe-images/$id"
          />
        )}
      />
    </>
  )
}
