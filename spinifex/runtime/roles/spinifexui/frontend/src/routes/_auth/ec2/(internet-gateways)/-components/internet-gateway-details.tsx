import type { InternetGateway } from "@aws-sdk/client-ec2"
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
import { useDeleteInternetGateway } from "@/mutations/ec2"

import { InternetGatewayAttachment } from "./internet-gateway-attachment"

// A gateway with no attachment reports none; AWS's console calls that detached.
export function attachmentState(igw: InternetGateway): string {
  return igw.Attachments?.[0]?.State ?? "detached"
}

function Overview({
  igw,
  gateways,
}: {
  igw: InternetGateway
  gateways: readonly InternetGateway[]
}) {
  return (
    <>
      <PanelSection title="Details">
        <PanelFields>
          <PanelField copy={igw.InternetGatewayId} label="Gateway ID" mono>
            {igw.InternetGatewayId}
          </PanelField>
          <PanelField label="State">{attachmentState(igw)}</PanelField>
          <PanelField label="Owner" mono>
            {igw.OwnerId}
          </PanelField>
        </PanelFields>
      </PanelSection>
      <InternetGatewayAttachment gateways={gateways} igw={igw} />
    </>
  )
}

function Settings({
  igw,
  onDeleted,
}: {
  igw: InternetGateway
  onDeleted: () => void
}) {
  const id = igw.InternetGatewayId ?? ""
  const attached = !!igw.Attachments?.[0]?.VpcId
  const remove = useDeleteInternetGateway()
  const [confirm, setConfirm] = useState(false)

  return (
    <>
      <Ec2Tags
        id={igw.InternetGatewayId}
        invalidate={["ec2", "internetGateways"]}
        tags={igw.Tags}
      />
      <PanelSection title="Manage">
        <div className="flex flex-col items-start gap-3">
          {attached && (
            <p className="text-sm text-muted-foreground">
              Detach it from its VPC in the Overview tab before deleting it.
            </p>
          )}
          <Button
            disabled={attached || remove.isPending}
            onClick={() => {
              setConfirm(true)
            }}
            size="sm"
            variant="destructive"
          >
            <Trash2 data-icon="inline-start" />
            Delete internet gateway
          </Button>
        </div>
      </PanelSection>
      <ResourceDeleteDialog
        description={`Delete ${id}? This cannot be undone.`}
        identifier={id}
        mutation={remove}
        onDeleted={onDeleted}
        onOpenChange={setConfirm}
        open={confirm}
        title="Delete internet gateway"
      />
    </>
  )
}

interface InternetGatewayDetailsProps {
  igw: InternetGateway
  // Every gateway, so attach offers only VPCs that have none.
  gateways: readonly InternetGateway[]
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  // Called on close and after a delete.
  onClose: () => void
  layout?: "auto" | "page"
}

// One gateway's details, as the list's side panel or as its own page.
export function InternetGatewayDetails({
  igw,
  gateways,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: InternetGatewayDetailsProps) {
  const id = igw.InternetGatewayId ?? ""
  const name = getNameTag(igw.Tags)

  return (
    <DetailPanel
      closeLabel={
        layout === "page"
          ? "Back to internet gateways"
          : "Close internet gateway details"
      }
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open internet gateway page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ id }}
            search={{ tab }}
            to="/ec2/describe-internet-gateways/$id"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="Internet gateway details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      subtitle={name ? id : undefined}
      summary={<StateBadge state={attachmentState(igw)} />}
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          content: <Overview gateways={gateways} igw={igw} />,
        },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: <Settings igw={igw} onDeleted={onClose} />,
        },
      ]}
      title={name ?? id}
    />
  )
}
