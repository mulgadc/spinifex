import type { Policy } from "@aws-sdk/client-iam"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import { Eye, Maximize2, SlidersHorizontal, Trash2 } from "lucide-react"
import { Suspense, useState } from "react"

import {
  DetailPanel,
  PanelField,
  PanelFields,
  PanelSection,
  PanelSpinner,
  PanelTags,
} from "@/components/detail-panel"
import { ResourceDeleteDialog } from "@/components/resource-delete-dialog"
import { Button, buttonVariants } from "@/components/ui/button"
import { decodePolicyDocument } from "@/lib/json"
import type { PanelTab } from "@/lib/panel-search"
import { formatDateTime } from "@/lib/utils"
import { useDeletePolicy } from "@/mutations/iam"
import {
  iamPolicyQueryOptions,
  iamPolicyVersionQueryOptions,
} from "@/queries/iam"

import { iamDeleteWarning } from "../../-components/delete-prerequisites"
import { PolicyDocumentViewer } from "../../-components/policy-document-viewer"

function PolicyDocument({ policy }: { policy: Policy }) {
  const { data } = useSuspenseQuery(
    iamPolicyVersionQueryOptions(
      policy.Arn ?? "",
      policy.DefaultVersionId ?? "v1",
    ),
  )

  const document = data.PolicyVersion?.Document

  return (
    <PanelSection title="Policy document">
      {document ? (
        <PolicyDocumentViewer document={decodePolicyDocument(document)} />
      ) : (
        <p className="text-sm text-muted-foreground">No document.</p>
      )}
    </PanelSection>
  )
}

// ListPolicies carries no tags, so they come from GetPolicy.
function Tags({ policyArn }: { policyArn: string }) {
  const { data } = useSuspenseQuery(iamPolicyQueryOptions(policyArn))

  return <PanelTags tags={data.Policy?.Tags} />
}

function Overview({ policy }: { policy: Policy }) {
  return (
    <>
      <PanelSection title="Details">
        <PanelFields>
          <PanelField label="Policy name">{policy.PolicyName}</PanelField>
          <PanelField copy={policy.PolicyId} label="Policy ID" mono>
            {policy.PolicyId}
          </PanelField>
          <PanelField copy={policy.Arn} label="ARN" mono>
            {policy.Arn}
          </PanelField>
          <PanelField label="Path" mono>
            {policy.Path}
          </PanelField>
          <PanelField label="Description">{policy.Description}</PanelField>
          <PanelField label="Default version" mono>
            {policy.DefaultVersionId}
          </PanelField>
          <PanelField label="Attachments">
            {String(policy.AttachmentCount ?? 0)}
          </PanelField>
          <PanelField label="Created">
            {formatDateTime(policy.CreateDate)}
          </PanelField>
          <PanelField label="Updated">
            {formatDateTime(policy.UpdateDate)}
          </PanelField>
        </PanelFields>
      </PanelSection>
      <Suspense fallback={<PanelSpinner />}>
        <PolicyDocument policy={policy} />
      </Suspense>
    </>
  )
}

function Settings({
  policy,
  onDeleted,
}: {
  policy: Policy
  onDeleted: () => void
}) {
  const policyArn = policy.Arn ?? ""
  const remove = useDeletePolicy()
  const [confirm, setConfirm] = useState(false)

  return (
    <>
      <Suspense fallback={<PanelSpinner />}>
        <Tags policyArn={policyArn} />
      </Suspense>
      <PanelSection title="Manage">
        <div className="flex flex-col items-start gap-3">
          <p className="text-sm text-muted-foreground">
            Detach it from every user, group and role before deleting it.
          </p>
          <Button
            disabled={remove.isPending}
            onClick={() => {
              setConfirm(true)
            }}
            size="sm"
            variant="destructive"
          >
            <Trash2 data-icon="inline-start" />
            Delete policy
          </Button>
        </div>
      </PanelSection>
      <ResourceDeleteDialog
        description={`Delete the policy ${policy.PolicyName ?? policyArn}? ${iamDeleteWarning("policy")}`}
        identifier={policyArn}
        mutation={remove}
        onDeleted={onDeleted}
        onOpenChange={setConfirm}
        open={confirm}
        title="Delete policy"
      />
    </>
  )
}

interface PolicyDetailsProps {
  policy: Policy
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  // Called on close and after a delete.
  onClose: () => void
  layout?: "auto" | "page"
}

// One managed policy's details, as the list's side panel or as its own page.
export function PolicyDetails({
  policy,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: PolicyDetailsProps) {
  const policyArn = policy.Arn ?? ""

  return (
    <DetailPanel
      closeLabel={
        layout === "page" ? "Back to policies" : "Close policy details"
      }
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open policy page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ policyArn: encodeURIComponent(policyArn) }}
            to="/iam/list-policies/$policyArn"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="Policy details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      subtitle={policy.PolicyId}
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          content: <Overview policy={policy} />,
        },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: <Settings onDeleted={onClose} policy={policy} />,
        },
      ]}
      title={policy.PolicyName ?? policyArn}
    />
  )
}
