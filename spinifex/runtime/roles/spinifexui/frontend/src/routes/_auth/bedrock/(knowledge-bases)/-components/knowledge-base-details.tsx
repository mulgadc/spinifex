import type { KnowledgeBaseSummary } from "@aws-sdk/client-bedrock-agent"
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
} from "@/components/detail-panel"
import { ResourceDeleteDialog } from "@/components/resource-delete-dialog"
import { StateBadge } from "@/components/state-badge"
import { Button, buttonVariants } from "@/components/ui/button"
import type { PanelTab } from "@/lib/panel-search"
import { formatDateTime } from "@/lib/utils"
import { useDeleteKnowledgeBase } from "@/mutations/bedrockAgent"
import {
  dataSourcesQueryOptions,
  knowledgeBaseQueryOptions,
} from "@/queries/bedrockAgent"

import { AddDataSourceForm } from "./add-data-source-form"
import { DataSourceCard } from "./data-source-card"
import { RetrieveTester } from "./retrieve-tester"

// What the panel needs before GetKnowledgeBase answers: the list row's fields.
export type KnowledgeBaseRow = Pick<
  KnowledgeBaseSummary,
  "knowledgeBaseId" | "name" | "status"
>

function DataSources({ knowledgeBaseId }: { knowledgeBaseId: string }) {
  const { data } = useSuspenseQuery(dataSourcesQueryOptions(knowledgeBaseId))
  const dataSources = data.dataSourceSummaries ?? []

  return (
    <PanelSection title="Data sources">
      <div className="flex flex-col gap-3">
        <div>
          <AddDataSourceForm knowledgeBaseId={knowledgeBaseId} />
        </div>
        {dataSources.length > 0 ? (
          <ul className="divide-y">
            {dataSources.map((dataSource) => (
              <DataSourceCard
                dataSource={dataSource}
                key={dataSource.dataSourceId}
                knowledgeBaseId={knowledgeBaseId}
              />
            ))}
          </ul>
        ) : (
          <p className="text-sm text-muted-foreground">
            No data sources configured for this knowledge base.
          </p>
        )}
      </div>
    </PanelSection>
  )
}

// GetKnowledgeBase carries the role and embedding model; the list does not.
function OverviewBody({ knowledgeBaseId }: { knowledgeBaseId: string }) {
  const { data } = useSuspenseQuery(knowledgeBaseQueryOptions(knowledgeBaseId))
  const { knowledgeBase } = data

  if (!knowledgeBase) {
    return (
      <p className="text-sm text-muted-foreground">Knowledge base not found.</p>
    )
  }

  const embeddingModelArn =
    knowledgeBase.knowledgeBaseConfiguration?.vectorKnowledgeBaseConfiguration
      ?.embeddingModelArn

  return (
    <>
      <PanelSection title="Details">
        <PanelFields>
          <PanelField label="Name">{knowledgeBase.name}</PanelField>
          <PanelField
            copy={knowledgeBase.knowledgeBaseId}
            label="Knowledge base ID"
            mono
          >
            {knowledgeBase.knowledgeBaseId}
          </PanelField>
          <PanelField copy={knowledgeBase.knowledgeBaseArn} label="ARN" mono>
            {knowledgeBase.knowledgeBaseArn}
          </PanelField>
          <PanelField label="Description">
            {knowledgeBase.description}
          </PanelField>
          <PanelField copy={knowledgeBase.roleArn} label="Role ARN" mono>
            {knowledgeBase.roleArn}
          </PanelField>
          <PanelField copy={embeddingModelArn} label="Embedding model" mono>
            {embeddingModelArn}
          </PanelField>
          <PanelField label="Created">
            {formatDateTime(knowledgeBase.createdAt)}
          </PanelField>
          <PanelField label="Updated">
            {formatDateTime(knowledgeBase.updatedAt)}
          </PanelField>
          {knowledgeBase.failureReasons &&
            knowledgeBase.failureReasons.length > 0 && (
              <PanelField label="Failure reasons">
                {knowledgeBase.failureReasons.join("; ")}
              </PanelField>
            )}
        </PanelFields>
      </PanelSection>
      <DataSources knowledgeBaseId={knowledgeBaseId} />
    </>
  )
}

function Settings({
  knowledgeBase,
  onDeleted,
}: {
  knowledgeBase: KnowledgeBaseRow
  onDeleted: () => void
}) {
  const knowledgeBaseId = knowledgeBase.knowledgeBaseId ?? ""
  const remove = useDeleteKnowledgeBase()
  const [confirm, setConfirm] = useState(false)

  return (
    <>
      <PanelSection title="Retrieve tester">
        <RetrieveTester knowledgeBaseId={knowledgeBaseId} />
      </PanelSection>
      <PanelSection title="Manage">
        <Button
          disabled={remove.isPending}
          onClick={() => {
            setConfirm(true)
          }}
          size="sm"
          variant="destructive"
        >
          <Trash2 data-icon="inline-start" />
          Delete knowledge base
        </Button>
      </PanelSection>
      <ResourceDeleteDialog
        description={`This tears down "${knowledgeBase.name ?? knowledgeBaseId}" and all of its data sources. It cannot be undone.`}
        identifier={knowledgeBaseId}
        mutation={remove}
        onDeleted={onDeleted}
        onOpenChange={setConfirm}
        open={confirm}
        title="Delete knowledge base"
      />
    </>
  )
}

interface KnowledgeBaseDetailsProps {
  knowledgeBase: KnowledgeBaseRow
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  // Called on close and after a delete.
  onClose: () => void
  layout?: "auto" | "page"
}

// One knowledge base's details, as the list's side panel or as its own page.
export function KnowledgeBaseDetails({
  knowledgeBase,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: KnowledgeBaseDetailsProps) {
  const knowledgeBaseId = knowledgeBase.knowledgeBaseId ?? ""

  return (
    <DetailPanel
      closeLabel={
        layout === "page"
          ? "Back to knowledge bases"
          : "Close knowledge base details"
      }
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open knowledge base page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ knowledgeBaseId }}
            to="/bedrock/list-knowledge-bases/$knowledgeBaseId"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="Knowledge base details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      subtitle={knowledgeBaseId}
      summary={<StateBadge state={knowledgeBase.status} />}
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          content: (
            <Suspense fallback={<PanelSpinner />}>
              <OverviewBody knowledgeBaseId={knowledgeBaseId} />
            </Suspense>
          ),
        },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: (
            <Settings knowledgeBase={knowledgeBase} onDeleted={onClose} />
          ),
        },
      ]}
      title={knowledgeBase.name ?? knowledgeBaseId}
    />
  )
}
