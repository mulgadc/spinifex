import type { Address } from "@aws-sdk/client-ec2"
import { useQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import { Link2, Unlink } from "lucide-react"
import { useState } from "react"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { PanelSection } from "@/components/detail-panel"
import { ErrorBanner } from "@/components/error-banner"
import { Button } from "@/components/ui/button"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { getNameTag } from "@/lib/utils"
import { useAssociateAddress, useDisassociateAddress } from "@/mutations/ec2"
import { ec2InstancesQueryOptions } from "@/queries/ec2"

// Associate and disassociate. Only running and stopped instances can take an
// address, so the picker offers nothing else.
export function ElasticIpAssociation({ address }: { address: Address }) {
  const allocationId = address.AllocationId ?? ""
  const associationId = address.AssociationId
  const { data } = useQuery(ec2InstancesQueryOptions)
  const associate = useAssociateAddress()
  const disassociate = useDisassociateAddress()
  const [target, setTarget] = useState("")
  const [confirm, setConfirm] = useState(false)

  const instances = (data?.Reservations ?? [])
    .flatMap((reservation) => reservation.Instances ?? [])
    .filter(
      (instance) =>
        instance.State?.Name === "running" ||
        instance.State?.Name === "stopped",
    )

  const error = associate.error ?? disassociate.error

  async function onAssociate() {
    try {
      await associate.mutateAsync({ allocationId, instanceId: target })
      setTarget("")
    } catch {
      // Shown by the banner above.
    }
  }

  async function onDisassociate() {
    if (!associationId) {
      return
    }

    try {
      await disassociate.mutateAsync(associationId)
    } catch {
      // Shown by the banner above.
    } finally {
      setConfirm(false)
    }
  }

  return (
    <PanelSection title="Association">
      <div className="flex flex-col gap-3">
        {error && <ErrorBanner error={error} msg="Elastic IP action failed" />}
        {associationId ? (
          <div className="flex items-center justify-between gap-3">
            {address.InstanceId ? (
              <Link
                className="truncate font-mono text-xs text-primary hover:underline"
                params={{ id: address.InstanceId }}
                to="/ec2/describe-instances/$id"
              >
                {address.InstanceId}
              </Link>
            ) : (
              <span className="truncate font-mono text-xs">
                {address.NetworkInterfaceId ?? associationId}
              </span>
            )}
            <Button
              disabled={disassociate.isPending}
              onClick={() => {
                setConfirm(true)
              }}
              size="sm"
              variant="outline"
            >
              <Unlink data-icon="inline-start" />
              Disassociate
            </Button>
          </div>
        ) : (
          <div className="flex items-center gap-2">
            <Select
              onValueChange={(value) => {
                setTarget(value ?? "")
              }}
              value={target}
            >
              <SelectTrigger
                aria-label="Instance to associate"
                className="min-w-0 flex-1"
              >
                <SelectValue
                  placeholder={
                    instances.length
                      ? "Select an instance"
                      : "No instance can take it"
                  }
                />
              </SelectTrigger>
              <SelectContent>
                {instances.map((instance) => {
                  const name = getNameTag(instance.Tags)

                  return (
                    <SelectItem
                      key={instance.InstanceId}
                      value={instance.InstanceId ?? ""}
                    >
                      {name
                        ? `${instance.InstanceId} (${name})`
                        : instance.InstanceId}
                    </SelectItem>
                  )
                })}
              </SelectContent>
            </Select>
            <Button
              disabled={!target || associate.isPending}
              onClick={() => {
                void onAssociate()
              }}
              size="sm"
            >
              <Link2 data-icon="inline-start" />
              Associate
            </Button>
          </div>
        )}
      </div>
      <DeleteConfirmationDialog
        confirmLabel="Disassociate"
        description={`Disassociate "${address.PublicIp}" from "${address.InstanceId ?? associationId}"? The instance stops being reachable at this address.`}
        isPending={disassociate.isPending}
        onConfirm={() => {
          void onDisassociate()
        }}
        onOpenChange={setConfirm}
        open={confirm}
        pendingLabel="Disassociating…"
        title="Disassociate Elastic IP"
      />
    </PanelSection>
  )
}
