import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import { BackLink } from "@/components/back-link"
import { panelSearchFields } from "@/lib/panel-search"
import { ec2TransitGatewayVpcAttachmentQueryOptions } from "@/queries/ec2"

import { TransitGatewayVpcAttachmentDetails } from "../-components/transit-gateway-vpc-attachment-details"

export const Route = createFileRoute(
  "/_auth/ec2/(transit-gateway-attachments)/describe-transit-gateway-vpc-attachments/$id",
)({
  validateSearch: z.object({ tab: panelSearchFields.tab }),
  search: { middlewares: [stripSearchParams({ tab: "overview" })] },
  loader: async ({ context, params }) => {
    await context.queryClient.query({
      ...ec2TransitGatewayVpcAttachmentQueryOptions(params.id),
      staleTime: "static",
    })
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `${params.id} | Transit Gateway Attachment`,
      },
    ],
  }),
  component: TransitGatewayVpcAttachmentDetail,
})

// The attachment's own page: the side panel's tabs, laid out for the full width.
function TransitGatewayVpcAttachmentDetail() {
  const { id } = Route.useParams()
  const { tab } = Route.useSearch()
  const navigate = Route.useNavigate()

  const { data } = useSuspenseQuery(
    ec2TransitGatewayVpcAttachmentQueryOptions(id),
  )

  const attachment = data.TransitGatewayVpcAttachments?.[0]

  if (!attachment?.TransitGatewayAttachmentId) {
    return (
      <>
        <BackLink to="/ec2/describe-transit-gateway-vpc-attachments">
          Back to transit gateway attachments
        </BackLink>
        <p className="text-muted-foreground">
          Transit gateway attachment not found.
        </p>
      </>
    )
  }

  return (
    <TransitGatewayVpcAttachmentDetails
      attachment={attachment}
      layout="page"
      onClose={() => {
        void navigate({ to: "/ec2/describe-transit-gateway-vpc-attachments" })
      }}
      onTabChange={(next) => {
        void navigate({ search: { tab: next }, replace: true })
      }}
      tab={tab}
    />
  )
}
