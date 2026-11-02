import type { InternetGateway } from "@aws-sdk/client-ec2"
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
import { vpcLabel } from "@/lib/utils"
import {
  useAttachInternetGateway,
  useDetachInternetGateway,
} from "@/mutations/ec2"
import { ec2VpcsQueryOptions } from "@/queries/ec2"

// Attach and detach, as the gateway's own page offers them. A VPC takes one
// gateway, so only VPCs without one are offered.
export function InternetGatewayAttachment({
  igw,
  gateways,
}: {
  igw: InternetGateway
  gateways: readonly InternetGateway[]
}) {
  const id = igw.InternetGatewayId ?? ""
  const vpcId = igw.Attachments?.[0]?.VpcId
  const { data } = useQuery(ec2VpcsQueryOptions)
  const attach = useAttachInternetGateway()
  const detach = useDetachInternetGateway()
  const [target, setTarget] = useState("")
  const [confirm, setConfirm] = useState(false)

  const taken = new Set(
    gateways.flatMap((gateway) =>
      (gateway.Attachments ?? []).map((attachment) => attachment.VpcId),
    ),
  )

  const free = (data?.Vpcs ?? []).filter((vpc) => !taken.has(vpc.VpcId))
  const error = attach.error ?? detach.error

  async function onAttach() {
    try {
      await attach.mutateAsync({ internetGatewayId: id, vpcId: target })
      setTarget("")
    } catch {
      // Shown by the banner above.
    }
  }

  async function onDetach() {
    if (!vpcId) {
      return
    }

    try {
      await detach.mutateAsync({ internetGatewayId: id, vpcId })
    } catch {
      // Shown by the banner above.
    } finally {
      setConfirm(false)
    }
  }

  return (
    <PanelSection title="VPC attachment">
      <div className="flex flex-col gap-3">
        {error && (
          <ErrorBanner error={error} msg="Internet gateway action failed" />
        )}
        {vpcId ? (
          <div className="flex items-center justify-between gap-3">
            <Link
              className="truncate font-mono text-xs text-primary hover:underline"
              params={{ id: vpcId }}
              to="/ec2/describe-vpcs/$id"
            >
              {vpcId}
            </Link>
            <Button
              disabled={detach.isPending}
              onClick={() => {
                setConfirm(true)
              }}
              size="sm"
              variant="outline"
            >
              <Unlink data-icon="inline-start" />
              Detach
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
                aria-label="VPC to attach"
                className="min-w-0 flex-1"
              >
                <SelectValue
                  placeholder={free.length ? "Select a VPC" : "No VPC is free"}
                />
              </SelectTrigger>
              <SelectContent>
                {free.map((vpc) => (
                  <SelectItem key={vpc.VpcId} value={vpc.VpcId ?? ""}>
                    {vpcLabel(vpc)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <Button
              disabled={!target || attach.isPending}
              onClick={() => {
                void onAttach()
              }}
              size="sm"
            >
              <Link2 data-icon="inline-start" />
              Attach
            </Button>
          </div>
        )}
      </div>
      <DeleteConfirmationDialog
        confirmLabel="Detach"
        description={`Detach ${id} from ${vpcId}? Every subnet in the VPC that routes through it loses internet access.`}
        isPending={detach.isPending}
        onConfirm={() => {
          void onDetach()
        }}
        onOpenChange={setConfirm}
        open={confirm}
        pendingLabel="Detaching…"
        title="Detach internet gateway"
      />
    </PanelSection>
  )
}
