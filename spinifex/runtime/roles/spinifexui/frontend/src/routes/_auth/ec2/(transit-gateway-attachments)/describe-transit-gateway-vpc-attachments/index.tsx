import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, stripSearchParams } from "@tanstack/react-router"
import { z } from "zod"

import {
  CLOSED_PANEL,
  type PanelSearch,
  panelSearchFields,
} from "@/lib/panel-search"
import { ec2TransitGatewayVpcAttachmentsQueryOptions } from "@/queries/ec2"

import { TransitGatewayVpcAttachmentsPage } from "../-components/transit-gateway-vpc-attachments-page"

const searchSchema = z.object(panelSearchFields)

export const Route = createFileRoute(
  "/_auth/ec2/(transit-gateway-attachments)/describe-transit-gateway-vpc-attachments/",
)({
  validateSearch: searchSchema,
  // Defaults stay out of the URL, so the plain list keeps its plain address.
  search: { middlewares: [stripSearchParams(CLOSED_PANEL)] },
  loader: async ({ context }) => {
    await context.queryClient.query({
      ...ec2TransitGatewayVpcAttachmentsQueryOptions,
      staleTime: "static",
    })
  },
  head: () => ({
    meta: [
      {
        title: "Transit Gateway Attachments | EC2 | Mulga",
      },
    ],
  }),
  component: TransitGatewayVpcAttachments,
})

function TransitGatewayVpcAttachments() {
  const { data } = useSuspenseQuery(ec2TransitGatewayVpcAttachmentsQueryOptions)
  const search = Route.useSearch()
  const navigate = Route.useNavigate()

  // Opening or closing a panel adds a history entry, so Back closes it.
  function onSearchChange(next: Partial<PanelSearch>) {
    void navigate({
      search: (previous) => ({ ...previous, ...next }),
      replace: !("selected" in next || "create" in next),
    })
  }

  return (
    <TransitGatewayVpcAttachmentsPage
      attachments={data.TransitGatewayVpcAttachments ?? []}
      onSearchChange={onSearchChange}
      search={search}
    />
  )
}
