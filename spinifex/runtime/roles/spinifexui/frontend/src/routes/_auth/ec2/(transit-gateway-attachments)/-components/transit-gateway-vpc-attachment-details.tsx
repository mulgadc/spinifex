import type { TransitGatewayVpcAttachment } from "@aws-sdk/client-ec2"
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
import { enabledLabel, isGoneState } from "@/lib/transit-gateway"
import { formatDateTime, getNameTag } from "@/lib/utils"
import { useDeleteTransitGatewayVpcAttachment } from "@/mutations/ec2"

function Overview({ attachment }: { attachment: TransitGatewayVpcAttachment }) {
  const subnets = attachment.SubnetIds ?? []
  const options = attachment.Options

  return (
    <>
      <PanelSection title="Details">
        <PanelFields>
          <PanelField
            copy={attachment.TransitGatewayAttachmentId}
            label="Attachment ID"
            mono
          >
            {attachment.TransitGatewayAttachmentId}
          </PanelField>
          <PanelField label="State">{attachment.State}</PanelField>
          <PanelField label="Transit gateway" mono>
            {attachment.TransitGatewayId && (
              <Link
                className="text-primary hover:underline"
                params={{ id: attachment.TransitGatewayId }}
                to="/ec2/describe-transit-gateways/$id"
              >
                {attachment.TransitGatewayId}
              </Link>
            )}
          </PanelField>
          <PanelField label="VPC" mono>
            {attachment.VpcId && (
              <Link
                className="text-primary hover:underline"
                params={{ id: attachment.VpcId }}
                to="/ec2/describe-vpcs/$id"
              >
                {attachment.VpcId}
              </Link>
            )}
          </PanelField>
          <PanelField copy={attachment.VpcOwnerId} label="VPC owner" mono>
            {attachment.VpcOwnerId}
          </PanelField>
          <PanelField label="Created">
            {attachment.CreationTime && formatDateTime(attachment.CreationTime)}
          </PanelField>
        </PanelFields>
      </PanelSection>
      <PanelSection title={`Subnets (${subnets.length})`}>
        {subnets.length === 0 ? (
          <p className="text-sm text-muted-foreground">No subnets.</p>
        ) : (
          <ul className="flex flex-col gap-1">
            {subnets.map((id) => (
              <li key={id}>
                <Link
                  className="font-mono text-xs text-primary hover:underline"
                  params={{ id }}
                  to="/ec2/describe-subnets/$id"
                >
                  {id}
                </Link>
              </li>
            ))}
          </ul>
        )}
      </PanelSection>
      <PanelSection title="Options">
        <PanelFields>
          <PanelField label="DNS support">
            {enabledLabel(options?.DnsSupport)}
          </PanelField>
          <PanelField label="Security group referencing">
            {enabledLabel(options?.SecurityGroupReferencingSupport)}
          </PanelField>
          <PanelField label="IPv6 support">
            {enabledLabel(options?.Ipv6Support)}
          </PanelField>
          <PanelField label="Appliance mode">
            {enabledLabel(options?.ApplianceModeSupport)}
          </PanelField>
        </PanelFields>
      </PanelSection>
    </>
  )
}

function Settings({
  attachment,
  onDeleted,
}: {
  attachment: TransitGatewayVpcAttachment
  onDeleted: () => void
}) {
  const id = attachment.TransitGatewayAttachmentId ?? ""
  const remove = useDeleteTransitGatewayVpcAttachment()
  const [confirm, setConfirm] = useState(false)

  return (
    <>
      <Ec2Tags
        id={attachment.TransitGatewayAttachmentId}
        invalidate={["ec2", "transitGatewayVpcAttachments"]}
        tags={attachment.Tags}
      />
      <PanelSection title="Manage">
        <Button
          disabled={isGoneState(attachment.State) || remove.isPending}
          onClick={() => {
            setConfirm(true)
          }}
          size="sm"
          variant="destructive"
        >
          <Trash2 data-icon="inline-start" />
          Delete attachment
        </Button>
      </PanelSection>
      <ResourceDeleteDialog
        description={`Delete ${id}? Traffic between ${attachment.VpcId ?? "the VPC"} and ${attachment.TransitGatewayId ?? "the transit gateway"} stops, and routes to the gateway from that VPC become blackholes. This cannot be undone.`}
        identifier={id}
        mutation={remove}
        onDeleted={onDeleted}
        onOpenChange={setConfirm}
        open={confirm}
        title="Delete attachment"
      />
    </>
  )
}

interface TransitGatewayVpcAttachmentDetailsProps {
  attachment: TransitGatewayVpcAttachment
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  // Called on close and after a delete.
  onClose: () => void
  layout?: "auto" | "page"
}

// One VPC attachment's details, as the list's side panel or as its own page.
export function TransitGatewayVpcAttachmentDetails({
  attachment,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: TransitGatewayVpcAttachmentDetailsProps) {
  const id = attachment.TransitGatewayAttachmentId ?? ""
  const name = getNameTag(attachment.Tags)

  return (
    <DetailPanel
      closeLabel={
        layout === "page"
          ? "Back to transit gateway attachments"
          : "Close attachment details"
      }
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open attachment page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ id }}
            search={{ tab }}
            to="/ec2/describe-transit-gateway-vpc-attachments/$id"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="Attachment details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      subtitle={name ? id : undefined}
      summary={<StateBadge state={attachment.State} />}
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          content: <Overview attachment={attachment} />,
        },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: <Settings attachment={attachment} onDeleted={onClose} />,
        },
      ]}
      title={name ?? id}
    />
  )
}
