import type { GuardrailSummary, GuardrailTopic } from "@aws-sdk/client-bedrock"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Link, useNavigate } from "@tanstack/react-router"
import { Pencil, Trash2 } from "lucide-react"
import { useState } from "react"

import { BackLink } from "@/components/back-link"
import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import { PageHeading } from "@/components/page-heading"
import { StateBadge } from "@/components/state-badge"
import { Button, buttonVariants } from "@/components/ui/button"
import { Field, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
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

interface GuardrailDetailPageProps {
  guardrailId: string
}

function DeniedTopicRow({ topic }: { topic: GuardrailTopic }) {
  return (
    <div className="rounded-md border p-3 text-sm">
      <div className="mb-1 flex items-center justify-between gap-2">
        <span className="font-semibold">{topic.name}</span>
        <span className="rounded-full bg-muted px-2 py-0.5 text-xs">
          {topic.type ?? "DENY"}
        </span>
      </div>
      <p className="text-muted-foreground">{topic.definition}</p>
      {topic.examples && topic.examples.length > 0 && (
        <ul className="mt-2 list-inside list-disc text-xs text-muted-foreground">
          {topic.examples.map((example) => (
            <li key={example}>{example}</li>
          ))}
        </ul>
      )}
    </div>
  )
}

function VersionRow({ version }: { version: GuardrailSummary }) {
  return (
    <div className="flex items-center justify-between rounded-md border p-2 text-sm">
      <span className="font-mono">{version.version}</span>
      <StateBadge state={version.status} />
    </div>
  )
}

// Cuts an immutable snapshot of the current DRAFT. The description is
// optional, so the field is plain local state rather than a validated form.
function CreateVersionForm({ guardrailId }: { guardrailId: string }) {
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
    <div className="rounded-lg border bg-card p-4">
      <h2 className="mb-2 font-semibold">Create version</h2>
      <p className="mb-4 text-sm text-muted-foreground">
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
        className="mt-3"
        disabled={createVersion.isPending}
        onClick={() => {
          void handleCreate()
        }}
        size="sm"
      >
        {createVersion.isPending ? "Creating…" : "Create version"}
      </Button>
      {createVersion.error && (
        <p className="mt-2 text-sm text-destructive">
          {createVersion.error.message}
        </p>
      )}
      {createVersion.isSuccess && (
        <p className="mt-2 text-sm text-muted-foreground">
          Version {createVersion.data.version} created.
        </p>
      )}
    </div>
  )
}

export function GuardrailDetailPage({ guardrailId }: GuardrailDetailPageProps) {
  const navigate = useNavigate()
  const [showDelete, setShowDelete] = useState(false)
  const { data: guardrail } = useSuspenseQuery(
    guardrailQueryOptions(guardrailId),
  )
  const { data: versionsData } = useSuspenseQuery(
    guardrailVersionsQueryOptions(guardrailId),
  )
  const deleteGuardrail = useDeleteGuardrail()

  const versions = versionsData.guardrails ?? []
  const deniedTopics = guardrail.topicPolicy?.topics ?? []

  async function handleDelete() {
    try {
      await deleteGuardrail.mutateAsync(guardrailId)
      setShowDelete(false)
      await navigate({ to: "/bedrock/list-guardrails" })
    } catch {
      // Left open so the refusal in the dialog description stays readable.
    }
  }

  return (
    <>
      <BackLink to="/bedrock/list-guardrails">Back to guardrails</BackLink>

      <div className="space-y-6">
        <PageHeading
          actions={
            <div className="flex items-center gap-2">
              <StateBadge state={guardrail.status} />
              <Link
                className={buttonVariants({ size: "sm", variant: "outline" })}
                params={{ guardrailId }}
                to="/bedrock/list-guardrails/$guardrailId/edit"
              >
                <Pencil className="size-4" />
                Edit
              </Link>
              <Button
                onClick={() => {
                  setShowDelete(true)
                }}
                size="sm"
                variant="destructive"
              >
                <Trash2 className="size-4" />
                Delete
              </Button>
            </div>
          }
          subtitle="Guardrail Details"
          title={guardrail.name ?? guardrailId}
        />

        <DetailCard>
          <DetailCard.Header>Guardrail Information</DetailCard.Header>
          <DetailCard.Content>
            <DetailRow label="Guardrail ID" value={guardrail.guardrailId} />
            <DetailRow label="ARN" value={guardrail.guardrailArn} />
            <DetailRow label="Version" value={guardrail.version} />
            <DetailRow
              label="Description"
              value={guardrail.description ?? "-"}
            />
            <DetailRow
              label="Created"
              value={formatDateTime(guardrail.createdAt)}
            />
            <DetailRow
              label="Updated"
              value={formatDateTime(guardrail.updatedAt)}
            />
            {guardrail.statusReasons && guardrail.statusReasons.length > 0 && (
              <DetailRow
                label="Status Reasons"
                value={guardrail.statusReasons.join("; ")}
              />
            )}
          </DetailCard.Content>
        </DetailCard>

        <div>
          <h2 className="mb-3 font-semibold">Denied topics</h2>
          {deniedTopics.length > 0 ? (
            <div className="space-y-2">
              {deniedTopics.map((topic) => (
                <DeniedTopicRow key={topic.name} topic={topic} />
              ))}
            </div>
          ) : (
            <p className="text-muted-foreground">
              No topic policy configured for this guardrail.
            </p>
          )}
        </div>

        <div>
          <h2 className="mb-3 font-semibold">Other policies</h2>
          <div className="flex flex-wrap gap-2">
            <span className="rounded-full bg-muted px-2 py-1 text-xs">
              Word policy:{" "}
              {(guardrail.wordPolicy?.words?.length ?? 0) > 0 ||
              (guardrail.wordPolicy?.managedWordLists?.length ?? 0) > 0
                ? "configured"
                : "not configured"}
            </span>
            <span className="rounded-full bg-muted px-2 py-1 text-xs">
              Sensitive information policy:{" "}
              {(guardrail.sensitiveInformationPolicy?.piiEntities?.length ??
                0) > 0 ||
              (guardrail.sensitiveInformationPolicy?.regexes?.length ?? 0) > 0
                ? "configured"
                : "not configured"}
            </span>
          </div>
        </div>

        <div>
          <h2 className="mb-3 font-semibold">Versions</h2>
          {versions.length > 0 ? (
            <div className="space-y-2">
              {versions.map((version) => (
                <VersionRow key={version.version} version={version} />
              ))}
            </div>
          ) : (
            <p className="text-muted-foreground">
              No versions found for this guardrail.
            </p>
          )}
        </div>

        <CreateVersionForm guardrailId={guardrailId} />

        <GuardrailTester
          guardrailId={guardrailId}
          guardrailVersion={guardrail.version ?? "DRAFT"}
        />
      </div>

      <DeleteConfirmationDialog
        description={
          <>
            {`This tears down "${guardrail.name ?? guardrailId}" and all of its versions. It cannot be undone.`}
            {deleteGuardrail.error && (
              <span className="mt-2 block text-destructive">
                {deleteGuardrail.error.message}
              </span>
            )}
          </>
        }
        isPending={deleteGuardrail.isPending}
        onConfirm={() => {
          void handleDelete()
        }}
        onOpenChange={setShowDelete}
        open={showDelete}
        title="Delete Guardrail"
      />
    </>
  )
}
