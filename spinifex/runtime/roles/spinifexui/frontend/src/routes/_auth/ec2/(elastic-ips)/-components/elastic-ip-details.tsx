import type { Address } from "@aws-sdk/client-ec2"
import { Link } from "@tanstack/react-router"
import { Eye, Maximize2, SlidersHorizontal, Trash2 } from "lucide-react"
import { useState } from "react"

import {
  DetailPanel,
  PanelField,
  PanelFields,
  PanelSection,
} from "@/components/detail-panel"
import { ResourceDeleteDialog } from "@/components/resource-delete-dialog"
import { Ec2Tags } from "@/components/resource-tags"
import { StateBadge } from "@/components/state-badge"
import { Button, buttonVariants } from "@/components/ui/button"
import type { PanelTab } from "@/lib/panel-search"
import { getNameTag } from "@/lib/utils"
import { useReleaseAddress } from "@/mutations/ec2"

import { ElasticIpAssociation } from "./elastic-ip-association"

// DescribeAddresses has no state field; the console reads it from the
// association.
export function associationState(address: Address): string {
  return address.AssociationId ? "associated" : "available"
}

function Overview({ address }: { address: Address }) {
  return (
    <>
      <PanelSection title="Details">
        <PanelFields>
          <PanelField copy={address.PublicIp} label="Public IP" mono>
            {address.PublicIp}
          </PanelField>
          <PanelField copy={address.AllocationId} label="Allocation ID" mono>
            {address.AllocationId}
          </PanelField>
          <PanelField label="Domain">{address.Domain}</PanelField>
          <PanelField label="Network border group">
            {address.NetworkBorderGroup}
          </PanelField>
          <PanelField label="Public IPv4 pool" mono>
            {address.PublicIpv4Pool}
          </PanelField>
        </PanelFields>
      </PanelSection>
      <PanelSection title="Associated with">
        <PanelFields>
          <PanelField copy={address.AssociationId} label="Association ID" mono>
            {address.AssociationId}
          </PanelField>
          <PanelField label="Instance" mono>
            {address.InstanceId && (
              <Link
                className="text-primary hover:underline"
                params={{ id: address.InstanceId }}
                to="/ec2/describe-instances/$id"
              >
                {address.InstanceId}
              </Link>
            )}
          </PanelField>
          <PanelField label="Network interface" mono>
            {address.NetworkInterfaceId}
          </PanelField>
          <PanelField copy={address.PrivateIpAddress} label="Private IP" mono>
            {address.PrivateIpAddress}
          </PanelField>
        </PanelFields>
      </PanelSection>
      <ElasticIpAssociation address={address} />
    </>
  )
}

function Settings({
  address,
  onReleased,
}: {
  address: Address
  onReleased: () => void
}) {
  const id = address.AllocationId ?? ""
  const associated = !!address.AssociationId
  const release = useReleaseAddress()
  const [confirm, setConfirm] = useState(false)

  return (
    <>
      <Ec2Tags
        id={address.AllocationId}
        invalidate={["ec2", "addresses"]}
        tags={address.Tags}
      />
      <PanelSection title="Manage">
        <div className="flex flex-col items-start gap-3">
          {associated && (
            <p className="text-sm text-muted-foreground">
              Disassociate it in the Overview tab before releasing it.
            </p>
          )}
          <Button
            disabled={associated || release.isPending}
            onClick={() => {
              setConfirm(true)
            }}
            size="sm"
            variant="destructive"
          >
            <Trash2 data-icon="inline-start" />
            Release Elastic IP
          </Button>
        </div>
      </PanelSection>
      <ResourceDeleteDialog
        confirmLabel="Release"
        description={`Release ${id} (${address.PublicIp})? The address goes back to the pool and cannot be recovered.`}
        identifier={id}
        mutation={release}
        onDeleted={onReleased}
        onOpenChange={setConfirm}
        open={confirm}
        pendingLabel="Releasing…"
        title="Release Elastic IP"
      />
    </>
  )
}

interface ElasticIpDetailsProps {
  address: Address
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  // Called on close and after a release.
  onClose: () => void
  layout?: "auto" | "page"
}

// One Elastic IP's details, as the list's side panel or as its own page.
export function ElasticIpDetails({
  address,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: ElasticIpDetailsProps) {
  const id = address.AllocationId ?? ""
  const name = getNameTag(address.Tags)

  return (
    <DetailPanel
      closeLabel={
        layout === "page" ? "Back to Elastic IPs" : "Close Elastic IP details"
      }
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open Elastic IP page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ id }}
            search={{ tab }}
            to="/ec2/describe-addresses/$id"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="Elastic IP details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      subtitle={name ? `${address.PublicIp} · ${id}` : id}
      summary={<StateBadge state={associationState(address)} />}
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          content: <Overview address={address} />,
        },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: <Settings address={address} onReleased={onClose} />,
        },
      ]}
      title={name ?? address.PublicIp ?? id}
    />
  )
}
