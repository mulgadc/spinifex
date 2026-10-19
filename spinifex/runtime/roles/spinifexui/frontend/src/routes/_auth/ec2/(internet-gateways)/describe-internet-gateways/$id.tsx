import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link } from "@tanstack/react-router"
import { useState } from "react"

import { BackLink } from "@/components/back-link"
import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { DeleteResourceButton } from "@/components/delete-resource-button"
import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import { ErrorBanner } from "@/components/error-banner"
import { PageHeading } from "@/components/page-heading"
import { Button } from "@/components/ui/button"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { getNameTag } from "@/lib/utils"
import {
  useAttachInternetGateway,
  useDeleteInternetGateway,
  useDetachInternetGateway,
} from "@/mutations/ec2"
import {
  ec2InternetGatewayQueryOptions,
  ec2VpcsQueryOptions,
} from "@/queries/ec2"

export const Route = createFileRoute(
  "/_auth/ec2/(internet-gateways)/describe-internet-gateways/$id",
)({
  loader: async ({ context, params }) => {
    await Promise.all([
      context.queryClient.query({
        ...ec2InternetGatewayQueryOptions(params.id),
        staleTime: "static",
      }),
      context.queryClient.query({
        ...ec2VpcsQueryOptions,
        staleTime: "static",
      }),
    ])
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `${params.id} | Internet Gateway | Mulga`,
      },
    ],
  }),
  component: InternetGatewayDetail,
})

function InternetGatewayDetail() {
  const { id } = Route.useParams()
  const { data } = useSuspenseQuery(ec2InternetGatewayQueryOptions(id))
  const { data: vpcsData } = useSuspenseQuery(ec2VpcsQueryOptions)
  const attachMutation = useAttachInternetGateway()
  const detachMutation = useDetachInternetGateway()
  const deleteMutation = useDeleteInternetGateway()
  const [vpcToDetach, setVpcToDetach] = useState<string | null>(null)
  const [selectedVpcId, setSelectedVpcId] = useState("")

  const igw = data.InternetGateways?.[0]
  const vpcs = vpcsData.Vpcs ?? []

  const handleAttach = async () => {
    if (!selectedVpcId) {
      return
    }

    await attachMutation.mutateAsync({
      internetGatewayId: id,
      vpcId: selectedVpcId,
    })
  }

  const handleDetach = async () => {
    if (!vpcToDetach) {
      return
    }

    try {
      await detachMutation.mutateAsync({
        internetGatewayId: id,
        vpcId: vpcToDetach,
      })
    } catch {
      // Error is stored in detachMutation.error and rendered above
    } finally {
      setVpcToDetach(null)
    }
  }

  if (!igw?.InternetGatewayId) {
    return (
      <>
        <BackLink to="/ec2/describe-internet-gateways">
          Back to Internet Gateways
        </BackLink>
        <p className="text-muted-foreground">Internet Gateway not found.</p>
      </>
    )
  }

  const attachment = igw.Attachments?.[0]
  const isAttached = !!attachment?.VpcId
  const name = getNameTag(igw.Tags)

  return (
    <>
      <BackLink to="/ec2/describe-internet-gateways">
        Back to Internet Gateways
      </BackLink>

      {(attachMutation.error ?? detachMutation.error) && (
        <ErrorBanner
          error={attachMutation.error ?? detachMutation.error ?? undefined}
          msg="Internet Gateway action failed"
        />
      )}

      <div className="space-y-6">
        <PageHeading
          actions={
            <DeleteResourceButton
              description={`Are you sure you want to delete the Internet Gateway "${igw.InternetGatewayId}"? This action cannot be undone.`}
              disabled={isAttached}
              id={id}
              mutation={deleteMutation}
              title="Delete Internet Gateway"
              to="/ec2/describe-internet-gateways"
            />
          }
          subtitle="Internet Gateway Details"
          title={
            name ? `${igw.InternetGatewayId} (${name})` : igw.InternetGatewayId
          }
        />

        <DetailCard>
          <DetailCard.Header>Internet Gateway Information</DetailCard.Header>
          <DetailCard.Content>
            <DetailRow
              label="Internet Gateway ID"
              value={igw.InternetGatewayId}
            />
            <DetailRow label="Owner ID" value={igw.OwnerId ?? "—"} />
            <DetailRow label="State" value={attachment?.State ?? "detached"} />
          </DetailCard.Content>
        </DetailCard>

        <DetailCard>
          <DetailCard.Header>VPC Attachment</DetailCard.Header>
          <DetailCard.Content>
            {isAttached ? (
              <div className="flex items-center justify-between">
                <Link
                  className="text-primary hover:underline"
                  params={{ id: attachment.VpcId ?? "" }}
                  to="/ec2/describe-vpcs/$id"
                >
                  {attachment.VpcId}
                </Link>
                <Button
                  disabled={detachMutation.isPending}
                  onClick={() => {
                    setVpcToDetach(attachment.VpcId ?? "")
                  }}
                  size="sm"
                  variant="outline"
                >
                  Detach
                </Button>
              </div>
            ) : (
              <div className="flex items-end gap-2">
                <div className="flex-1">
                  <Select
                    onValueChange={(value) => {
                      setSelectedVpcId(value ?? "")
                    }}
                    value={selectedVpcId}
                  >
                    <SelectTrigger className="w-full">
                      <SelectValue placeholder="Select a VPC" />
                    </SelectTrigger>
                    <SelectContent>
                      {vpcs.map((vpc) => {
                        const vpcName = getNameTag(vpc.Tags)

                        return (
                          <SelectItem key={vpc.VpcId} value={vpc.VpcId ?? ""}>
                            {vpcName
                              ? `${vpc.VpcId} (${vpcName})`
                              : `${vpc.VpcId} (${vpc.CidrBlock})`}
                          </SelectItem>
                        )
                      })}
                    </SelectContent>
                  </Select>
                </div>
                <Button
                  disabled={!selectedVpcId || attachMutation.isPending}
                  onClick={handleAttach}
                  size="sm"
                >
                  Attach
                </Button>
              </div>
            )}
          </DetailCard.Content>
        </DetailCard>
      </div>

      <DeleteConfirmationDialog
        confirmLabel="Detach"
        description={`Detach "${igw.InternetGatewayId}" from "${vpcToDetach}"? Every subnet in the VPC that routes through it loses internet access.`}
        isPending={detachMutation.isPending}
        onConfirm={handleDetach}
        onOpenChange={(open) => {
          if (!open) {
            setVpcToDetach(null)
          }
        }}
        open={vpcToDetach !== null}
        pendingLabel="Detaching…"
        title="Detach Internet Gateway"
      />
    </>
  )
}
