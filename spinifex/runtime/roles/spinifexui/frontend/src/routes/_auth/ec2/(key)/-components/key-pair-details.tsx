import type { KeyPairInfo } from "@aws-sdk/client-ec2"
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
import { Button, buttonVariants } from "@/components/ui/button"
import type { PanelTab } from "@/lib/panel-search"
import { formatDateTime } from "@/lib/utils"
import { useDeleteKeyPair } from "@/mutations/ec2"

function Overview({ keyPair }: { keyPair: KeyPairInfo }) {
  return (
    <PanelSection title="Details">
      <PanelFields>
        <PanelField copy={keyPair.KeyPairId} label="Key pair ID" mono>
          {keyPair.KeyPairId}
        </PanelField>
        <PanelField label="Name">{keyPair.KeyName}</PanelField>
        <PanelField label="Type">{keyPair.KeyType}</PanelField>
        <PanelField copy={keyPair.KeyFingerprint} label="Fingerprint" mono>
          {keyPair.KeyFingerprint && (
            <span title={keyPair.KeyFingerprint}>{keyPair.KeyFingerprint}</span>
          )}
        </PanelField>
        <PanelField label="Created">
          {formatDateTime(keyPair.CreateTime)}
        </PanelField>
      </PanelFields>
    </PanelSection>
  )
}

function Settings({
  keyPair,
  onDeleted,
}: {
  keyPair: KeyPairInfo
  onDeleted: () => void
}) {
  const remove = useDeleteKeyPair()
  const [confirm, setConfirm] = useState(false)

  return (
    <>
      <PanelTags tags={keyPair.Tags} />
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
            Delete key pair
          </Button>
        </div>
      </PanelSection>
      <ResourceDeleteDialog
        description={`Delete ${keyPair.KeyName ?? keyPair.KeyPairId}? Instances already launched with it keep the public key, but it can no longer be chosen at launch. This cannot be undone.`}
        identifier={keyPair.KeyPairId ?? ""}
        mutation={remove}
        onDeleted={onDeleted}
        onOpenChange={setConfirm}
        open={confirm}
        title="Delete key pair"
      />
    </>
  )
}

interface KeyPairDetailsProps {
  keyPair: KeyPairInfo
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  // Called on close and after a delete.
  onClose: () => void
  layout?: "auto" | "page"
}

// One key pair's details, as the list's side panel or as its own page.
export function KeyPairDetails({
  keyPair,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: KeyPairDetailsProps) {
  const id = keyPair.KeyPairId ?? ""

  return (
    <DetailPanel
      closeLabel={
        layout === "page" ? "Back to key pairs" : "Close key pair details"
      }
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open key pair page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ id }}
            to="/ec2/describe-key-pairs/$id"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="Key pair details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      subtitle={keyPair.KeyName ? id : undefined}
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          content: <Overview keyPair={keyPair} />,
        },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: <Settings keyPair={keyPair} onDeleted={onClose} />,
        },
      ]}
      title={keyPair.KeyName ?? id}
    />
  )
}
