import type {
  GetGuardrailCommandOutput,
  GuardrailSummary,
  GuardrailTopic,
} from "@aws-sdk/client-bedrock"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import { Eye, Maximize2, Pencil, SlidersHorizontal, Trash2 } from "lucide-react"
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
import { Field, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import type { PanelTab } from "@/lib/panel-search"
import { formatDateTime } from "@/lib/utils"
import {
  useCreateGuardrailVersion,
  useDeleteGuardrail,
} from "@/mutations/bedrock"
import {
  guardrailQueryOptions,
  guardrailVersionsQueryOptions,
} from "@/queries/bedrock"

import { GuardrailTester } from "./guardrail-tester"

// What the panel needs before GetGuardrail answers: the list row's fields.
export type GuardrailRow = Pick<GuardrailSummary, "id" | "name" | "status">

function DeniedTopicRow({ topic }: { topic: GuardrailTopic }) {
  return (
    <li className="flex flex-col gap-1 py-2 text-sm">
      <div className="flex items-center justify-between gap-2">
        <span className="font-medium">{topic.name}</span>
        <span className="rounded-full bg-muted px-2 py-0.5 text-xs">
          {topic.type ?? "DENY"}
        </span>
      </div>
      <p className="text-muted-foreground">{topic.definition}</p>
      {topic.examples && topic.examples.length > 0 && (
        <ul className="list-inside list-disc text-xs text-muted-foreground">
          {topic.examples.map((example) => (
            <li key={example}>{example}</li>
          ))}
        </ul>
      )}
    </li>
  )
}

function configured(isConfigured: boolean): string {
  return isConfigured ? "configured" : "not configured"
}

function Policies({ guardrail }: { guardrail: GetGuardrailCommandOutput }) {
  const deniedTopics = guardrail.topicPolicy?.topics ?? []

  const hasWords =
    (guardrail.wordPolicy?.words?.length ?? 0) > 0 ||
    (guardrail.wordPolicy?.managedWordLists?.length ?? 0) > 0

  const hasSensitive =
    (guardrail.sensitiveInformationPolicy?.piiEntities?.length ?? 0) > 0 ||
    (guardrail.sensitiveInformationPolicy?.regexes?.length ?? 0) > 0

  return (
    <>
      <PanelSection title="Denied topics">
        {deniedTopics.length > 0 ? (
          <ul className="divide-y">
            {deniedTopics.map((topic) => (
              <DeniedTopicRow key={topic.name} topic={topic} />
            ))}
          </ul>
        ) : (
          <p className="text-sm text-muted-foreground">
            No topic policy configured for this guardrail.
          </p>
        )}
      </PanelSection>
      <PanelSection title="Other policies">
        <div className="flex flex-wrap gap-2">
          <span className="rounded-full bg-muted px-2 py-1 text-xs">
            Word policy: {configured(hasWords)}
          </span>
          <span className="rounded-full bg-muted px-2 py-1 text-xs">
            Sensitive information policy: {configured(hasSensitive)}
          </span>
        </div>
      </PanelSection>
    </>
  )
}

function Versions({ guardrailId }: { guardrailId: string }) {
  const { data } = useSuspenseQuery(guardrailVersionsQueryOptions(guardrailId))
  const versions = data.guardrails ?? []

  return (
    <PanelSection title="Versions">
      {versions.length > 0 ? (
        <PanelFields>
          {versions.map((version) => (
            <PanelField key={version.version} label={version.version ?? ""}>
              <StateBadge state={version.status} />
            </PanelField>
          ))}
        </PanelFields>
      ) : (
        <p className="text-sm text-muted-foreground">
          No versions found for this guardrail.
        </p>
      )}
    </PanelSection>
  )
}

// GetGuardrail carries the policies, which ListGuardrails does not.
function OverviewBody({ guardrailId }: { guardrailId: string }) {
  const { data: guardrail } = useSuspenseQuery(
    guardrailQueryOptions(guardrailId),
  )

  return (
    <>
      <PanelSection title="Details">
        <PanelFields>
          <PanelField label="Name">{guardrail.name}</PanelField>
          <PanelField copy={guardrail.guardrailId} label="Guardrail ID" mono>
            {guardrail.guardrailId}
          </PanelField>
          <PanelField copy={guardrail.guardrailArn} label="ARN" mono>
            {guardrail.guardrailArn}
          </PanelField>
          <PanelField label="Version" mono>
            {guardrail.version}
          </PanelField>
          <PanelField label="Description">{guardrail.description}</PanelField>
          <PanelField label="Created">
            {formatDateTime(guardrail.createdAt)}
          </PanelField>
          <PanelField label="Updated">
            {formatDateTime(guardrail.updatedAt)}
          </PanelField>
          {guardrail.statusReasons && guardrail.statusReasons.length > 0 && (
            <PanelField label="Status reasons">
              {guardrail.statusReasons.join("; ")}
            </PanelField>
          )}
        </PanelFields>
      </PanelSection>
      <Policies guardrail={guardrail} />
      <Versions guardrailId={guardrailId} />
    </>
  )
}

// Cuts an immutable snapshot of the current DRAFT. The description is
// optional, so the field is plain local state rather than a validated form.
function CreateVersion({ guardrailId }: { guardrailId: string }) {
  const [description, setDescription] = useState("")
  const createVersion = useCreateGuardrailVersion()

  async function handleCreate() {
    try {
      await createVersion.mutateAsync({
        description: description.length > 0 ? description : undefined,
        guardrailIdentifier: guardrailId,
      })
      setDescription("")
    } catch {
      // Surfaced below via createVersion.error
    }
  }

  return (
    <PanelSection title="New version">
      <div className="flex flex-col items-start gap-3">
        <p className="text-sm text-muted-foreground">
          Snapshots the current DRAFT as a new immutable version.
        </p>
        <Field>
          <FieldTitle>
            <label htmlFor="version-description">Description</label>
          </FieldTitle>
          <Input
            id="version-description"
            onChange={(e) => {
              setDescription(e.target.value)
            }}
            placeholder="Optional"
            value={description}
          />
        </Field>
        <Button
          disabled={createVersion.isPending}
          onClick={() => {
            void handleCreate()
          }}
          size="sm"
        >
          {createVersion.isPending ? "Creating…" : "Create version"}
        </Button>
        {createVersion.error && (
          <p className="text-sm text-destructive">
            {createVersion.error.message}
          </p>
        )}
        {createVersion.isSuccess && (
          <p className="text-sm text-muted-foreground">
            Version {createVersion.data.version} created.
          </p>
        )}
      </div>
    </PanelSection>
  )
}

function Tester({ guardrailId }: { guardrailId: string }) {
  const { data: guardrail } = useSuspenseQuery(
    guardrailQueryOptions(guardrailId),
  )

  return (
    <PanelSection title="Guardrail tester">
      <GuardrailTester
        guardrailId={guardrailId}
        guardrailVersion={guardrail.version ?? "DRAFT"}
      />
    </PanelSection>
  )
}

function Settings({
  guardrail,
  onDeleted,
}: {
  guardrail: GuardrailRow
  onDeleted: () => void
}) {
  const guardrailId = guardrail.id ?? ""
  const remove = useDeleteGuardrail()
  const [confirm, setConfirm] = useState(false)

  return (
    <>
      <PanelSection title="Configuration">
        <div className="flex flex-col items-start gap-3">
          <p className="text-sm text-muted-foreground">
            Change the DRAFT&apos;s topics, word filters and blocked messages.
          </p>
          <Link
            className={buttonVariants({ size: "sm", variant: "outline" })}
            params={{ guardrailId }}
            to="/bedrock/list-guardrails/$guardrailId/edit"
          >
            <Pencil data-icon="inline-start" />
            Edit
          </Link>
        </div>
      </PanelSection>
      <CreateVersion guardrailId={guardrailId} />
      <Suspense fallback={<PanelSpinner />}>
        <Tester guardrailId={guardrailId} />
      </Suspense>
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
          Delete guardrail
        </Button>
      </PanelSection>
      <ResourceDeleteDialog
        description={`This tears down "${guardrail.name ?? guardrailId}" and all of its versions. It cannot be undone.`}
        identifier={guardrailId}
        mutation={remove}
        onDeleted={onDeleted}
        onOpenChange={setConfirm}
        open={confirm}
        title="Delete guardrail"
      />
    </>
  )
}

interface GuardrailDetailsProps {
  guardrail: GuardrailRow
  tab: PanelTab
  onTabChange: (tab: PanelTab) => void
  // Called on close and after a delete.
  onClose: () => void
  layout?: "auto" | "page"
}

// One guardrail's details, as the list's side panel or as its own page.
export function GuardrailDetails({
  guardrail,
  tab,
  onTabChange,
  onClose,
  layout = "auto",
}: GuardrailDetailsProps) {
  const guardrailId = guardrail.id ?? ""

  return (
    <DetailPanel
      closeLabel={
        layout === "page" ? "Back to guardrails" : "Close guardrail details"
      }
      fullPage={
        layout === "auto" && (
          <Link
            aria-label="Open guardrail page"
            className={buttonVariants({ size: "icon-sm", variant: "ghost" })}
            params={{ guardrailId }}
            to="/bedrock/list-guardrails/$guardrailId"
          >
            <Maximize2 />
          </Link>
        )
      }
      label="Guardrail details"
      layout={layout}
      onClose={onClose}
      onTabChange={onTabChange}
      subtitle={guardrailId}
      summary={<StateBadge state={guardrail.status} />}
      tab={tab}
      tabs={[
        {
          id: "overview",
          label: "Overview",
          icon: Eye,
          content: (
            <Suspense fallback={<PanelSpinner />}>
              <OverviewBody guardrailId={guardrailId} />
            </Suspense>
          ),
        },
        {
          id: "settings",
          label: "Settings",
          icon: SlidersHorizontal,
          content: <Settings guardrail={guardrail} onDeleted={onClose} />,
        },
      ]}
      title={guardrail.name ?? guardrailId}
    />
  )
}
