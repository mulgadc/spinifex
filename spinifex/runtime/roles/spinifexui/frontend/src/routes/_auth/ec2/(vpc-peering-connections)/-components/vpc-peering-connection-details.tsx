import type {
  VpcPeeringConnection,
  VpcPeeringConnectionVpcInfo,
} from "@aws-sdk/client-ec2"
import { useQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import {
  Check,
  Eye,
  Maximize2,
  SlidersHorizontal,
  Trash2,
  X,
} from "lucide-react"
import { useState } from "react"

import {
  DetailPanel,
  PanelField,
  PanelFields,
  PanelSection,
} from "@/components/detail-panel"
import { ErrorBanner } from "@/components/error-banner"
import { ResourceDeleteDialog } from "@/components/resource-delete-dialog"
import { Ec2Tags } from "@/components/resource-tags"
import { StateBadge } from "@/components/state-badge"
import { useTopologyTab } from "@/components/topology/resource-topology"
import { Button, buttonVariants } from "@/components/ui/button"
import type { PanelTab } from "@/lib/panel-search"
import { formatDateTime, getNameTag } from "@/lib/utils"
import { canAnswer, canDelete, ownSide, sideInfo } from "@/lib/vpc-peering"
import {
  useAcceptVpcPeeringConnection,
  useDeleteVpcPeeringConnection,
  useRejectVpcPeeringConnection,
} from "@/mutations/ec2"
import { callerIdentityQueryOptions } from "@/queries/sts"

function Side({
  title,
  info,
  accountId,
}: {
  title: string
  info: VpcPeeringConnectionVpcInfo | undefined
  accountId: string | undefined
}) {
  const owned = info?.OwnerId !== undefined && info.OwnerId === accountId

  return (
    <PanelSection title={title}>
      <PanelFields>
        <PanelField copy={info?.VpcId} label="VPC" mono>
          {info?.VpcId &&
            (owned ? (
              <Link
                className="text-primary hover:underline"
                params={{ id: info.VpcId }}
                to="/ec2/describe-vpcs/$id"
              >
                {info.VpcId}
              </Link>
            ) : (
              info.VpcId
            ))}
        </PanelField>
        <PanelField label="CIDR" mono>
          {info?.CidrBlock}
        </PanelField>
        <PanelField copy={info?.OwnerId} label="Owner" mono>
          {info?.OwnerId}
        </PanelField>
        <PanelField label="Region">{info?.Region}</PanelField>
      </PanelFields>
    </PanelSection>
  )
}

// Accept and Reject, offered only to the accepter's owner while pending.
function Answer({ pcx }: { pcx: VpcPeeringConnection }) {
  const id = pcx.VpcPeeringConnectionId ?? ""
  const accept = useAcceptVpcPeeringConnection()
  const reject = useRejectVpcPeeringConnection()
  const busy = accept.isPending || reject.isPending
  const error = accept.error ?? reject.error

  return (
    <PanelSection title="Pending acceptance">
      <div className="flex flex-col gap-3">
        <p className="text-sm text-muted-foreground">
          {pcx.RequesterVpcInfo?.OwnerId} asks to peer{" "}
          {pcx.RequesterVpcInfo?.VpcId} with your VPC{" "}
          {pcx.AccepterVpcInfo?.VpcId}. Neither side routes over it until each
          adds a route.
        </p>
        {error && (
          <ErrorBanner error={error} msg="Failed to answer the request" />
        )}
        <div className="flex gap-2">
          <Button
            disabled={busy}
            onClick={() => {
              reject.reset()
              accept.mutate(id)
            }}
            size="sm"
          >
            <Check data-icon="inline-start" />
            Accept request
          </Button>
          <Button
            disabled={busy}
            onClick={() => {
              accept.reset()
              reject.mutate(id)
            }}
            size="sm"
            variant="outline"
          >
            <X data-icon="inline-start" />
            Reject request
          </Button>
        </div>
      </div>
    </PanelSection>
  )
}

function Overview({
  pcx,
  accountId,
}: {
  pcx: VpcPeeringConnection
  accountId: string | undefined
}) {
  const pending = pcx.Status?.Code === "pending-acceptance"

  return (
    <>
      {canAnswer(pcx, accountId) && <Answer pcx={pcx} />}
      <PanelSection title="Details">
        <PanelFields>
          <PanelField
            copy={pcx.VpcPeeringConnectionId}
            label="Peering connection ID"
            mono
          >
            {pcx.VpcPeeringConnectionId}
          </PanelField>
          <PanelField label="Status">{pcx.Status?.Code}</PanelField>
          <PanelField label="Message">{pcx.Status?.Message}</PanelField>
          {pending && (
            <PanelField label="Expires">
              {pcx.ExpirationTime && formatDateTime(pcx.ExpirationTime)}
            </PanelField>
          )}
        </PanelFields>
      </PanelSection>
      <Side
        accountId={accountId}
        info={pcx.RequesterVpcInfo}
        title="Requester"
      />
      <Side accountId={accountId} info={pcx.AccepterVpcInfo} title="Accepter" />
    </>
  )
}

function Settings({
  pcx,
  onDeleted,
}: {
  pcx: VpcPeeringConnection
  onDeleted: () => void
}) {
  const id = pcx.VpcPeeringConnectionId ?? ""
  const remove = useDeleteVpcPeeringConnection()
  const [confirm, setConfirm] = useState(false)

  return (
    <>
      <Ec2Tags
        id={pcx.VpcPeeringConnectionId}
        invalidate={["ec2", "vpcPeeringConnections"]}
        tags={pcx.Tags}
      />
      <PanelSection title="Manage">
        <Button
          disabled={!canDelete(pcx) || remove.isPending}
          onClick={() => {
            setConfirm(true)
          }}
          size="sm"
          variant="destructive"
        >
          <Trash2 data-icon="inline-start" />
          Delete peering connection
        </Button>
      </PanelSection>
      <ResourceDeleteDialog
        description={`Delete ${id}? Traffic between the two VPCs stops, and routes to it in either VPC become blackholes. This cannot be undone.`}
        identifier={id}
        mutation={remove}
        onDeleted={onDeleted}
        onOpenChange={setConfirm}
        open={confirm}
        title="Delete peering connection"
      />
    </>
  )
}

interface VpcPeeringConnectionDetailsProps {
  pcx: VpcPeeringConnection
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  // Called on close and after a delete.
  onClose: () => void
  layout?: "auto" | "page"
}

// One peering connection's details, as the list's side panel or its own page.
export function VpcPeeringConnectionDetails({
  pcx,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: VpcPeeringConnectionDetailsProps) {
  const id = pcx.VpcPeeringConnectionId ?? ""
  const name = getNameTag(pcx.Tags)
  const { data: identity } = useQuery(callerIdentityQueryOptions)
  const accountId = identity?.Account
  const side = ownSide(pcx, accountId)

  // The map is of the caller's own VPC, the only one it can read.
  const topology = useTopologyTab({
    page: layout === "page",
    tab,
    vpcId: side && sideInfo(pcx, side)?.VpcId,
    focus: { kind: "pcx", id },
    noVpc: identity
      ? "Neither VPC of this connection belongs to this account."
      : "Reading which VPC is yours…",
    pin: { peeringConnection: pcx },
  })

  return (
    <DetailPanel
      closeLabel={
        layout === "page"
          ? "Back to peering connections"
          : "Close peering connection details"
      }
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open peering connection page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ id }}
            search={{ tab }}
            to="/ec2/describe-vpc-peering-connections/$id"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="Peering connection details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      subtitle={name ? id : undefined}
      summary={
        <>
          <StateBadge state={pcx.Status?.Code} />
          {topology.toggle}
        </>
      }
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          content: <Overview accountId={accountId} pcx={pcx} />,
        },
        topology.spec,
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: <Settings onDeleted={onClose} pcx={pcx} />,
        },
      ]}
      title={name ?? id}
    />
  )
}
