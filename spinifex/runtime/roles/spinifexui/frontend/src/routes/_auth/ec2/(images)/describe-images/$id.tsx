import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { BackLink } from "@/components/back-link"
import { panelSearchFields } from "@/lib/panel-search"
import { isSystemManagedImage } from "@/lib/system-managed"
import { ec2ImageQueryOptions } from "@/queries/ec2"

import { ImageDetails } from "../-components/image-details"

export const Route = createFileRoute("/_auth/ec2/(images)/describe-images/$id")(
  {
    validateSearch: z.object({ tab: panelSearchFields.tab }),
    search: { middlewares: [stripSearchParams({ tab: "overview" })] },
    loader: async ({ context, params }) =>
      await context.queryClient.query({
        ...ec2ImageQueryOptions(params.id),
        staleTime: "static",
      }),
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

// The image's own page: the side panel's tabs, laid out for the full width.
function ImageDetail() {
  const { id } = Route.useParams()
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()
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
    <ImageDetails
      image={image}
      layout="page"
      onClose={() => {
        void navigate({ to: "/ec2/describe-images" })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      tab={tab}
    />
  )
}
