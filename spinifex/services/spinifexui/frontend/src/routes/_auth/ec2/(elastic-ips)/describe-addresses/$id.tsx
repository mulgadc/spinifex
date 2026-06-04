import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, useNavigate } from "@tanstack/react-router"
import { Trash2 } from "lucide-react"
import { useState } from "react"

import { BackLink } from "@/components/back-link"
import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
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
  useAssociateAddress,
  useDisassociateAddress,
  useReleaseAddress,
} from "@/mutations/ec2"
import { ec2AddressQueryOptions, ec2InstancesQueryOptions } from "@/queries/ec2"

export const Route = createFileRoute(
  "/_auth/ec2/(elastic-ips)/describe-addresses/$id",
)({
  loader: async ({ context, params }) => {
    await Promise.all([
      context.queryClient.ensureQueryData(ec2AddressQueryOptions(params.id)),
      context.queryClient.ensureQueryData(ec2InstancesQueryOptions),
    ])
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `${params.id} | Elastic IP | Mulga`,
      },
    ],
  }),
  component: AddressDetail,
})

function AddressDetail() {
  const { id } = Route.useParams()
  const navigate = useNavigate()
  const { data } = useSuspenseQuery(ec2AddressQueryOptions(id))
  const { data: instancesData } = useSuspenseQuery(ec2InstancesQueryOptions)
  const associateMutation = useAssociateAddress()
  const disassociateMutation = useDisassociateAddress()
  const releaseMutation = useReleaseAddress()
  const [showReleaseDialog, setShowReleaseDialog] = useState(false)
  const [selectedInstanceId, setSelectedInstanceId] = useState("")

  const address = data.Addresses?.[0]
  const instances =
    instancesData.Reservations?.flatMap((r) => r.Instances ?? []).filter(
      (i) => i.State?.Name === "running" || i.State?.Name === "stopped",
    ) ?? []

  const handleRelease = async () => {
    try {
      await releaseMutation.mutateAsync(id)
      navigate({ to: "/ec2/describe-addresses" })
    } finally {
      setShowReleaseDialog(false)
    }
  }

  const handleAssociate = async () => {
    if (!selectedInstanceId) {
      return
    }
    await associateMutation.mutateAsync({
      allocationId: id,
      instanceId: selectedInstanceId,
    })
  }

  const handleDisassociate = async () => {
    if (!address?.AssociationId) {
      return
    }
    await disassociateMutation.mutateAsync(address.AssociationId)
  }

  if (!address?.AllocationId) {
    return (
      <>
        <BackLink to="/ec2/describe-addresses">Back to Elastic IPs</BackLink>
        <p className="text-muted-foreground">Elastic IP not found.</p>
      </>
    )
  }

  const isAssociated = !!address.AssociationId
  const name = getNameTag(address.Tags)

  return (
    <>
      <BackLink to="/ec2/describe-addresses">Back to Elastic IPs</BackLink>

      {(releaseMutation.error ??
        associateMutation.error ??
        disassociateMutation.error) && (
        <ErrorBanner
          error={
            releaseMutation.error ??
            associateMutation.error ??
            disassociateMutation.error ??
            undefined
          }
          msg="Elastic IP action failed"
        />
      )}

      <div className="space-y-6">
        <PageHeading
          actions={
            <Button
              disabled={isAssociated || releaseMutation.isPending}
              onClick={() => setShowReleaseDialog(true)}
              size="sm"
              variant="destructive"
            >
              <Trash2 className="size-4" />
              Release
            </Button>
          }
          subtitle="Elastic IP Details"
          title={
            name ? `${address.AllocationId} (${name})` : address.AllocationId
          }
        />

        <DetailCard>
          <DetailCard.Header>Elastic IP Information</DetailCard.Header>
          <DetailCard.Content>
            <DetailRow label="Allocation ID" value={address.AllocationId} />
            <DetailRow label="Public IP" value={address.PublicIp} />
            <DetailRow label="Domain" value={address.Domain} />
            <DetailRow
              label="Association ID"
              value={address.AssociationId ?? "—"}
            />
            <DetailRow label="Instance ID" value={address.InstanceId ?? "—"} />
            <DetailRow
              label="Private IP"
              value={address.PrivateIpAddress ?? "—"}
            />
          </DetailCard.Content>
        </DetailCard>

        <DetailCard>
          <DetailCard.Header>Association</DetailCard.Header>
          <DetailCard.Content>
            {isAssociated ? (
              <Button
                disabled={disassociateMutation.isPending}
                onClick={handleDisassociate}
                size="sm"
                variant="outline"
              >
                Disassociate
              </Button>
            ) : (
              <div className="flex items-end gap-2">
                <div className="flex-1">
                  <Select
                    onValueChange={(value) =>
                      setSelectedInstanceId(value ?? "")
                    }
                    value={selectedInstanceId}
                  >
                    <SelectTrigger className="w-full">
                      <SelectValue placeholder="Select an instance" />
                    </SelectTrigger>
                    <SelectContent>
                      {instances.map((instance) => {
                        const instanceName = getNameTag(instance.Tags)
                        return (
                          <SelectItem
                            key={instance.InstanceId}
                            value={instance.InstanceId ?? ""}
                          >
                            {instanceName
                              ? `${instance.InstanceId} (${instanceName})`
                              : instance.InstanceId}
                          </SelectItem>
                        )
                      })}
                    </SelectContent>
                  </Select>
                </div>
                <Button
                  disabled={!selectedInstanceId || associateMutation.isPending}
                  onClick={handleAssociate}
                  size="sm"
                >
                  Associate
                </Button>
              </div>
            )}
          </DetailCard.Content>
        </DetailCard>
      </div>

      <DeleteConfirmationDialog
        description={`Are you sure you want to release the Elastic IP "${address.AllocationId}" (${address.PublicIp})? This action cannot be undone.`}
        isPending={releaseMutation.isPending}
        onConfirm={handleRelease}
        onOpenChange={setShowReleaseDialog}
        open={showReleaseDialog}
        title="Release Elastic IP"
      />
    </>
  )
}
