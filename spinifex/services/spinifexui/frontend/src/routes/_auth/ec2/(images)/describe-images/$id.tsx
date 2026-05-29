import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute } from "@tanstack/react-router"

import { BackLink } from "@/components/back-link"
import { PageHeading } from "@/components/page-heading"
import { StateBadge } from "@/components/state-badge"
import { isSystemManagedImage } from "@/lib/system-managed"
import { ec2ImageQueryOptions } from "@/queries/ec2"

import { AmiDetails } from "../../-components/ami-details"

export const Route = createFileRoute("/_auth/ec2/(images)/describe-images/$id")(
  {
    loader: async ({ context, params }) =>
      await context.queryClient.ensureQueryData(
        ec2ImageQueryOptions(params.id),
      ),
    head: ({ loaderData }) => ({
      meta: [
        {
          title: `${loaderData?.Images?.[0]?.Name ?? "Image"} | EC2 | Mulga`,
        },
      ],
    }),
    component: ImageDetail,
  },
)

function ImageDetail() {
  const { id } = Route.useParams()
  const { data } = useSuspenseQuery(ec2ImageQueryOptions(id))
  const image = data?.Images?.[0]

  if (!image?.ImageId || isSystemManagedImage(image)) {
    return (
      <>
        <BackLink to="/ec2/describe-images">Back to images</BackLink>
        <p className="text-muted-foreground">Image not found.</p>
      </>
    )
  }

  return (
    <>
      <BackLink to="/ec2/describe-images">Back to images</BackLink>

      <div className="space-y-6">
        <PageHeading
          actions={<StateBadge state={image.State} />}
          subtitle="Image Details"
          title={image.ImageId}
        />

        <AmiDetails image={image} showExtendedDetails />
      </div>
    </>
  )
}
