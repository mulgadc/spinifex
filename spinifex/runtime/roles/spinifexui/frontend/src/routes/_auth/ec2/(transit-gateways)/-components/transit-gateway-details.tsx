import type {
  TransitGateway,
  TransitGatewayVpcAttachment,
} from "@aws-sdk/client-ec2"
import { useQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import { Eye, Maximize2, SlidersHorizontal, Trash2 } from "lucide-react"
import { useState } from "react"

import { type DataColumn, DataTable } from "@/components/data-table"
import {
  DetailPanel,
  PanelField,
  PanelFields,
  PanelSection,
  PanelSpinner,
} from "@/components/detail-panel"
import { ErrorBanner } from "@/components/error-banner"
import { ResourceDeleteDialog } from "@/components/resource-delete-dialog"
import { Ec2Tags } from "@/components/resource-tags"
import { StateBadge } from "@/components/state-badge"
import { TruncatedText } from "@/components/table-cells"
import { Button, buttonVariants } from "@/components/ui/button"
import type { PanelTab } from "@/lib/panel-search"
import { enabledLabel, isGoneState } from "@/lib/transit-gateway"
import { formatDateTime, getNameTag } from "@/lib/utils"
import { useDeleteTransitGateway } from "@/mutations/ec2"
import { ec2GatewayTransitGatewayAttachmentsQueryOptions } from "@/queries/ec2"

const ATTACHMENT_COLUMNS: DataColumn<TransitGatewayVpcAttachment>[] = [
  {
    id: "attachment",
    header: "Attachment",
    cell: (att) => {
      const id = att.TransitGatewayAttachmentId ?? ""

      return (
        <Link
          className="block max-w-40 truncate font-mono text-xs text-primary hover:underline"
          params={{ id }}
          to="/ec2/describe-transit-gateway-vpc-attachments/$id"
        >
          {id}
        </Link>
      )
    },
  },
  {
    id: "vpc",
    header: "VPC",
    hideBelow: "sm",
    cell: (att) => (
      <TruncatedText className="max-w-32 font-mono text-xs" text={att.VpcId} />
    ),
  },
  {
    id: "state",
    header: "State",
    cell: (att) => <StateBadge state={att.State} />,
  },
]

function AttachmentList({ tgwId }: { tgwId: string }) {
  const query = useQuery(ec2GatewayTransitGatewayAttachmentsQueryOptions(tgwId))

  if (query.isPending) {
    return <PanelSpinner />
  }

  if (query.error) {
    return (
      <ErrorBanner error={query.error} msg="Failed to load VPC attachments" />
    )
  }

  // A deleted attachment stays visible to Describe for a while; it holds nothing.
  const attachments = (query.data.TransitGatewayVpcAttachments ?? []).filter(
    (att) => att.State !== "deleted",
  )

  return (
    <DataTable
      caption="VPC attachments"
      columns={ATTACHMENT_COLUMNS}
      empty="No VPC attachments. A VPC routes through this gateway once it is attached and its route tables name the gateway."
      getRowId={(att) => att.TransitGatewayAttachmentId ?? ""}
      rows={attachments}
    />
  )
}

function Attachments({ tgwId }: { tgwId: string }) {
  return (
    <PanelSection title="VPC attachments">
      <div className="flex flex-col gap-3">
        <AttachmentList tgwId={tgwId} />
        <div>
          <Link
            className={buttonVariants({ size: "sm", variant: "outline" })}
            search={{ create: true }}
            to="/ec2/describe-transit-gateway-vpc-attachments"
          >
            Create VPC attachment
          </Link>
        </div>
      </div>
    </PanelSection>
  )
}

function Overview({ tgw }: { tgw: TransitGateway }) {
  const options = tgw.Options

  return (
    <>
      <PanelSection title="Details">
        <PanelFields>
          <PanelField
            copy={tgw.TransitGatewayId}
            label="Transit gateway ID"
            mono
          >
            {tgw.TransitGatewayId}
          </PanelField>
          <PanelField label="State">{tgw.State}</PanelField>
          <PanelField label="Description">{tgw.Description}</PanelField>
          <PanelField copy={tgw.TransitGatewayArn} label="ARN" mono>
            {tgw.TransitGatewayArn}
          </PanelField>
          <PanelField copy={tgw.OwnerId} label="Owner" mono>
            {tgw.OwnerId}
          </PanelField>
          <PanelField label="Created">
            {tgw.CreationTime && formatDateTime(tgw.CreationTime)}
          </PanelField>
        </PanelFields>
      </PanelSection>
      <Attachments tgwId={tgw.TransitGatewayId ?? ""} />
      <PanelSection title="Options">
        <PanelFields>
          <PanelField label="Amazon side ASN" mono>
            {options?.AmazonSideAsn}
          </PanelField>
          <PanelField label="DNS support">
            {enabledLabel(options?.DnsSupport)}
          </PanelField>
          <PanelField label="VPN ECMP support">
            {enabledLabel(options?.VpnEcmpSupport)}
          </PanelField>
          <PanelField label="Security group referencing">
            {enabledLabel(options?.SecurityGroupReferencingSupport)}
          </PanelField>
          <PanelField label="Default route table association">
            {enabledLabel(options?.DefaultRouteTableAssociation)}
          </PanelField>
          <PanelField label="Default route table propagation">
            {enabledLabel(options?.DefaultRouteTablePropagation)}
          </PanelField>
          <PanelField
            copy={options?.AssociationDefaultRouteTableId}
            label="Default route table"
            mono
          >
            {options?.AssociationDefaultRouteTableId}
          </PanelField>
          <PanelField label="Auto accept shared attachments">
            {enabledLabel(options?.AutoAcceptSharedAttachments)}
          </PanelField>
          <PanelField label="Multicast support">
            {enabledLabel(options?.MulticastSupport)}
          </PanelField>
        </PanelFields>
      </PanelSection>
    </>
  )
}

function Settings({
  tgw,
  onDeleted,
}: {
  tgw: TransitGateway
  onDeleted: () => void
}) {
  const id = tgw.TransitGatewayId ?? ""
  const remove = useDeleteTransitGateway()
  const [confirm, setConfirm] = useState(false)

  return (
    <>
      <Ec2Tags
        id={tgw.TransitGatewayId}
        invalidate={["ec2", "transitGateways"]}
        tags={tgw.Tags}
      />
      <PanelSection title="Manage">
        <Button
          disabled={isGoneState(tgw.State) || remove.isPending}
          onClick={() => {
            setConfirm(true)
          }}
          size="sm"
          variant="destructive"
        >
          <Trash2 data-icon="inline-start" />
          Delete transit gateway
        </Button>
      </PanelSection>
      <ResourceDeleteDialog
        description={`Delete ${id}? A transit gateway with attachments is refused; delete its attachments first. This cannot be undone.`}
        identifier={id}
        mutation={remove}
        onDeleted={onDeleted}
        onOpenChange={setConfirm}
        open={confirm}
        title="Delete transit gateway"
      />
    </>
  )
}

interface TransitGatewayDetailsProps {
  tgw: TransitGateway
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  // Called on close and after a delete.
  onClose: () => void
  layout?: "auto" | "page"
}

// One transit gateway's details, as the list's side panel or as its own page.
export function TransitGatewayDetails({
  tgw,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: TransitGatewayDetailsProps) {
  const id = tgw.TransitGatewayId ?? ""
  const name = getNameTag(tgw.Tags)

  return (
    <DetailPanel
      closeLabel={
        layout === "page"
          ? "Back to transit gateways"
          : "Close transit gateway details"
      }
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open transit gateway page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ id }}
            search={{ tab }}
            to="/ec2/describe-transit-gateways/$id"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="Transit gateway details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      subtitle={name ? id : undefined}
      summary={<StateBadge state={tgw.State} />}
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          content: <Overview tgw={tgw} />,
        },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: <Settings onDeleted={onClose} tgw={tgw} />,
        },
      ]}
      title={name ?? id}
    />
  )
}
