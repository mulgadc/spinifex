import type { Subnet } from "@aws-sdk/client-ec2"
import { Link } from "@tanstack/react-router"
import { Eye, Maximize2, SlidersHorizontal, Trash2 } from "lucide-react"
import { useState } from "react"

import {
  DetailPanel,
  PanelField,
  PanelFields,
  PanelSection,
  PanelTags,
} from "@/components/detail-panel"
import { ResourceDeleteDialog } from "@/components/resource-delete-dialog"
import { StateBadge } from "@/components/state-badge"
import { Button, buttonVariants } from "@/components/ui/button"
import type { PanelTab } from "@/lib/panel-search"
import { getNameTag } from "@/lib/utils"
import { useDeleteSubnet } from "@/mutations/ec2"

function Overview({ subnet }: { subnet: Subnet }) {
  return (
    <PanelSection title="Details">
      <PanelFields>
        <PanelField copy={subnet.SubnetId} label="Subnet ID" mono>
          {subnet.SubnetId}
        </PanelField>
        <PanelField label="VPC" mono>
          {subnet.VpcId && (
            <Link
              className="text-primary hover:underline"
              params={{ id: subnet.VpcId }}
              to="/ec2/describe-vpcs/$id"
            >
              {subnet.VpcId}
            </Link>
          )}
        </PanelField>
        <PanelField copy={subnet.CidrBlock} label="CIDR" mono>
          {subnet.CidrBlock}
        </PanelField>
        <PanelField label="Availability zone">
          {subnet.AvailabilityZone}
        </PanelField>
        <PanelField label="Available IPv4 addresses">
          {subnet.AvailableIpAddressCount?.toString()}
        </PanelField>
        <PanelField label="Auto-assign public IPv4">
          {subnet.MapPublicIpOnLaunch ? "Yes" : "No"}
        </PanelField>
        <PanelField label="Default for AZ">
          {subnet.DefaultForAz ? "Yes" : "No"}
        </PanelField>
        <PanelField label="Owner" mono>
          {subnet.OwnerId}
        </PanelField>
      </PanelFields>
    </PanelSection>
  )
}

function Settings({
  subnet,
  onDeleted,
}: {
  subnet: Subnet
  onDeleted: () => void
}) {
  const id = subnet.SubnetId ?? ""
  const remove = useDeleteSubnet()
  const [confirm, setConfirm] = useState(false)

  return (
    <>
      <PanelTags tags={subnet.Tags} />
      <PanelSection title="Manage">
        <div className="flex flex-col items-start gap-3">
          <Button
            disabled={remove.isPending}
            onClick={() => {
              setConfirm(true)
            }}
            size="sm"
            variant="destructive"
          >
            <Trash2 data-icon="inline-start" />
            Delete subnet
          </Button>
        </div>
      </PanelSection>
      <ResourceDeleteDialog
        description={`Delete ${id}? It must have no network interfaces left. This cannot be undone.`}
        identifier={id}
        mutation={remove}
        onDeleted={onDeleted}
        onOpenChange={setConfirm}
        open={confirm}
        title="Delete subnet"
      />
    </>
  )
}

interface SubnetDetailsProps {
  subnet: Subnet
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  // Called on close and after a delete.
  onClose: () => void
  layout?: "auto" | "page"
}

// One subnet's details, as the list's side panel or as its own page.
export function SubnetDetails({
  subnet,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: SubnetDetailsProps) {
  const id = subnet.SubnetId ?? ""
  const name = getNameTag(subnet.Tags)

  return (
    <DetailPanel
      closeLabel={
        layout === "page" ? "Back to subnets" : "Close subnet details"
      }
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open subnet page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ id }}
            to="/ec2/describe-subnets/$id"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="Subnet details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      subtitle={name ? id : undefined}
      summary={<StateBadge state={subnet.State} />}
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          content: <Overview subnet={subnet} />,
        },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: <Settings onDeleted={onClose} subnet={subnet} />,
        },
      ]}
      title={name ?? id}
    />
  )
}
