import type { NatGateway } from "@aws-sdk/client-ec2"
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
import { formatDateTime, getNameTag } from "@/lib/utils"
import { useDeleteNatGateway } from "@/mutations/ec2"

function Overview({ nat }: { nat: NatGateway }) {
  const addresses = nat.NatGatewayAddresses ?? []

  return (
    <>
      <PanelSection title="Details">
        <PanelFields>
          <PanelField copy={nat.NatGatewayId} label="NAT gateway ID" mono>
            {nat.NatGatewayId}
          </PanelField>
          <PanelField label="State">{nat.State}</PanelField>
          {nat.FailureMessage && (
            <PanelField label="Failure">{nat.FailureMessage}</PanelField>
          )}
          <PanelField label="Connectivity">{nat.ConnectivityType}</PanelField>
          <PanelField label="Subnet" mono>
            {nat.SubnetId && (
              <Link
                className="text-primary hover:underline"
                params={{ id: nat.SubnetId }}
                to="/ec2/describe-subnets/$id"
              >
                {nat.SubnetId}
              </Link>
            )}
          </PanelField>
          <PanelField label="VPC" mono>
            {nat.VpcId && (
              <Link
                className="text-primary hover:underline"
                params={{ id: nat.VpcId }}
                to="/ec2/describe-vpcs/$id"
              >
                {nat.VpcId}
              </Link>
            )}
          </PanelField>
          <PanelField label="Created">
            {nat.CreateTime && formatDateTime(nat.CreateTime)}
          </PanelField>
        </PanelFields>
      </PanelSection>
      <PanelSection title="Addresses">
        {addresses.length === 0 ? (
          <p className="text-sm text-muted-foreground">No addresses.</p>
        ) : (
          addresses.map((address) => (
            <PanelFields key={address.AllocationId ?? address.PrivateIp}>
              <PanelField copy={address.PublicIp} label="Public IP" mono>
                {address.PublicIp}
              </PanelField>
              <PanelField copy={address.PrivateIp} label="Private IP" mono>
                {address.PrivateIp}
              </PanelField>
              <PanelField label="Allocation ID" mono>
                {address.AllocationId && (
                  <Link
                    className="text-primary hover:underline"
                    params={{ id: address.AllocationId }}
                    to="/ec2/describe-addresses/$id"
                  >
                    {address.AllocationId}
                  </Link>
                )}
              </PanelField>
            </PanelFields>
          ))
        )}
      </PanelSection>
    </>
  )
}

// A gateway already deleting or deleted has nothing left to delete.
function isGone(nat: NatGateway): boolean {
  return nat.State === "deleting" || nat.State === "deleted"
}

function Settings({
  nat,
  onDeleted,
}: {
  nat: NatGateway
  onDeleted: () => void
}) {
  const id = nat.NatGatewayId ?? ""
  const remove = useDeleteNatGateway()
  const [confirm, setConfirm] = useState(false)

  return (
    <>
      <PanelTags tags={nat.Tags} />
      <PanelSection title="Manage">
        <Button
          disabled={isGone(nat) || remove.isPending}
          onClick={() => {
            setConfirm(true)
          }}
          size="sm"
          variant="destructive"
        >
          <Trash2 data-icon="inline-start" />
          Delete NAT gateway
        </Button>
      </PanelSection>
      <ResourceDeleteDialog
        description={`Delete ${id}? This cannot be undone. Its Elastic IP is not released.`}
        identifier={id}
        mutation={remove}
        onDeleted={onDeleted}
        onOpenChange={setConfirm}
        open={confirm}
        title="Delete NAT gateway"
      />
    </>
  )
}

interface NatGatewayDetailsProps {
  nat: NatGateway
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  // Called on close and after a delete.
  onClose: () => void
  layout?: "auto" | "page"
}

// One NAT gateway's details, as the list's side panel or as its own page.
export function NatGatewayDetails({
  nat,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: NatGatewayDetailsProps) {
  const id = nat.NatGatewayId ?? ""
  const name = getNameTag(nat.Tags)

  return (
    <DetailPanel
      closeLabel={
        layout === "page" ? "Back to NAT gateways" : "Close NAT gateway details"
      }
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open NAT gateway page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ id }}
            search={{ tab }}
            to="/ec2/describe-nat-gateways/$id"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="NAT gateway details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      subtitle={name ? id : undefined}
      summary={<StateBadge state={nat.State} />}
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          content: <Overview nat={nat} />,
        },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: <Settings nat={nat} onDeleted={onClose} />,
        },
      ]}
      title={name ?? id}
    />
  )
}
