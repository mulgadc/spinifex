import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery } from "@tanstack/react-query"
import { useNavigate } from "@tanstack/react-router"
import { Trash2 } from "lucide-react"
import { useState } from "react"
import { useForm } from "react-hook-form"

import { BackLink } from "@/components/back-link"
import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import { ErrorBanner } from "@/components/error-banner"
import { PageHeading } from "@/components/page-heading"
import { StateBadge } from "@/components/state-badge"
import { Button } from "@/components/ui/button"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { formatDateTime } from "@/lib/utils"
import {
  useCreateDataSource,
  useDeleteKnowledgeBase,
} from "@/mutations/bedrockAgent"
import {
  dataSourcesQueryOptions,
  knowledgeBaseQueryOptions,
} from "@/queries/bedrockAgent"
import {
  type DataSourceFormData,
  dataSourceFormSchema,
  EMPTY_DATA_SOURCE_FORM_DEFAULTS,
} from "@/types/bedrockAgent"

import { DataSourceCard } from "./data-source-card"
import { RetrieveTester } from "./retrieve-tester"

interface KnowledgeBaseDetailPageProps {
  knowledgeBaseId: string
}

function AddDataSourceForm({ knowledgeBaseId }: { knowledgeBaseId: string }) {
  const [open, setOpen] = useState(false)
  const createDataSource = useCreateDataSource()

  const {
    formState: { errors, isSubmitting },
    handleSubmit,
    register,
    reset,
  } = useForm<DataSourceFormData>({
    resolver: zodResolver(dataSourceFormSchema),
    defaultValues: EMPTY_DATA_SOURCE_FORM_DEFAULTS,
  })

  const onSubmit = async (data: DataSourceFormData) => {
    await createDataSource.mutateAsync({ data, knowledgeBaseId })
    reset(EMPTY_DATA_SOURCE_FORM_DEFAULTS)
    setOpen(false)
  }

  if (!open) {
    return (
      <Button
        onClick={() => {
          setOpen(true)
        }}
        size="sm"
        variant="outline"
      >
        Add data source
      </Button>
    )
  }

  return (
    <div className="rounded-lg border bg-card p-4">
      <h3 className="mb-3 font-medium">Add data source</h3>

      {createDataSource.error && (
        <ErrorBanner
          error={createDataSource.error}
          msg="Failed to create the data source"
        />
      )}

      <form
        className="space-y-4"
        onSubmit={(event) => {
          void handleSubmit(onSubmit)(event)
        }}
      >
        <Field>
          <FieldTitle>
            <label htmlFor="ds-name">Name</label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors.name}
            id="ds-name"
            placeholder="product-docs-bucket"
            {...register("name")}
          />
          <FieldError errors={[errors.name]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="ds-bucket-arn">S3 bucket ARN</label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors.bucketArn}
            id="ds-bucket-arn"
            placeholder="arn:aws:s3:::product-docs"
            {...register("bucketArn")}
          />
          <FieldError errors={[errors.bucketArn]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="ds-inclusion-prefix">Inclusion prefix</label>
          </FieldTitle>
          <Input
            id="ds-inclusion-prefix"
            placeholder="Optional"
            {...register("inclusionPrefix")}
          />
        </Field>

        <Field>
          <FieldTitle>Chunking</FieldTitle>
          <label className="flex items-center gap-2 text-xs">
            <input
              aria-label="Enable fixed-size chunking"
              type="checkbox"
              {...register("chunkingEnabled")}
            />
            <span>Split documents using fixed-size chunking</span>
          </label>
        </Field>

        <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
          <Field>
            <FieldTitle>
              <label htmlFor="ds-max-tokens">Max tokens per chunk</label>
            </FieldTitle>
            <Input
              aria-invalid={!!errors.maxTokens}
              id="ds-max-tokens"
              min={1}
              type="number"
              {...register("maxTokens", { valueAsNumber: true })}
            />
            <FieldError errors={[errors.maxTokens]} />
          </Field>
          <Field>
            <FieldTitle>
              <label htmlFor="ds-overlap-percentage">Overlap %</label>
            </FieldTitle>
            <Input
              aria-invalid={!!errors.overlapPercentage}
              id="ds-overlap-percentage"
              max={99}
              min={0}
              type="number"
              {...register("overlapPercentage", { valueAsNumber: true })}
            />
            <FieldError errors={[errors.overlapPercentage]} />
          </Field>
        </div>

        <div className="flex gap-2">
          <Button
            disabled={isSubmitting || createDataSource.isPending}
            onClick={() => {
              reset(EMPTY_DATA_SOURCE_FORM_DEFAULTS)
              setOpen(false)
            }}
            type="button"
            variant="outline"
          >
            Cancel
          </Button>
          <Button
            disabled={isSubmitting || createDataSource.isPending}
            type="submit"
          >
            {isSubmitting || createDataSource.isPending
              ? "Adding…"
              : "Add data source"}
          </Button>
        </div>
      </form>
    </div>
  )
}

export function KnowledgeBaseDetailPage({
  knowledgeBaseId,
}: KnowledgeBaseDetailPageProps) {
  const navigate = useNavigate()
  const [showDelete, setShowDelete] = useState(false)
  const { data: kbData } = useSuspenseQuery(
    knowledgeBaseQueryOptions(knowledgeBaseId),
  )
  const { data: dataSourcesData } = useSuspenseQuery(
    dataSourcesQueryOptions(knowledgeBaseId),
  )
  const deleteKnowledgeBase = useDeleteKnowledgeBase()

  const { knowledgeBase } = kbData
  const dataSources = dataSourcesData.dataSourceSummaries ?? []

  if (!knowledgeBase) {
    return (
      <>
        <BackLink to="/bedrock/list-knowledge-bases">
          Back to knowledge bases
        </BackLink>
        <p className="text-muted-foreground">Knowledge base not found.</p>
      </>
    )
  }

  const embeddingModelArn =
    knowledgeBase.knowledgeBaseConfiguration?.vectorKnowledgeBaseConfiguration
      ?.embeddingModelArn

  async function handleDelete() {
    try {
      await deleteKnowledgeBase.mutateAsync(knowledgeBaseId)
      setShowDelete(false)
      await navigate({ to: "/bedrock/list-knowledge-bases" })
    } catch {
      // Left open so the refusal in the dialog description stays readable.
    }
  }

  return (
    <>
      <BackLink to="/bedrock/list-knowledge-bases">
        Back to knowledge bases
      </BackLink>

      <div className="space-y-6">
        <PageHeading
          actions={
            <div className="flex items-center gap-2">
              <StateBadge state={knowledgeBase.status} />
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
          subtitle="Knowledge Base Details"
          title={knowledgeBase.name ?? knowledgeBaseId}
        />

        <DetailCard>
          <DetailCard.Header>Knowledge Base Information</DetailCard.Header>
          <DetailCard.Content>
            <DetailRow
              label="Knowledge Base ID"
              value={knowledgeBase.knowledgeBaseId}
            />
            <DetailRow label="ARN" value={knowledgeBase.knowledgeBaseArn} />
            <DetailRow
              label="Description"
              value={knowledgeBase.description ?? "-"}
            />
            <DetailRow label="Role ARN" value={knowledgeBase.roleArn} />
            <DetailRow label="Embedding Model" value={embeddingModelArn} />
            <DetailRow
              label="Created"
              value={formatDateTime(knowledgeBase.createdAt)}
            />
            <DetailRow
              label="Updated"
              value={formatDateTime(knowledgeBase.updatedAt)}
            />
            {knowledgeBase.failureReasons &&
              knowledgeBase.failureReasons.length > 0 && (
                <DetailRow
                  label="Failure Reasons"
                  value={knowledgeBase.failureReasons.join("; ")}
                />
              )}
          </DetailCard.Content>
        </DetailCard>

        <div>
          <div className="mb-3 flex items-center justify-between">
            <h2 className="font-semibold">Data Sources</h2>
            <AddDataSourceForm knowledgeBaseId={knowledgeBaseId} />
          </div>
          {dataSources.length > 0 ? (
            <div className="space-y-4">
              {dataSources.map((dataSource) => (
                <DataSourceCard
                  dataSource={dataSource}
                  key={dataSource.dataSourceId}
                  knowledgeBaseId={knowledgeBaseId}
                />
              ))}
            </div>
          ) : (
            <p className="text-muted-foreground">
              No data sources configured for this knowledge base.
            </p>
          )}
        </div>

        <RetrieveTester knowledgeBaseId={knowledgeBaseId} />
      </div>

      <DeleteConfirmationDialog
        description={
          <>
            {`This tears down "${knowledgeBase.name ?? knowledgeBaseId}" and all of its data sources. It cannot be undone.`}
            {deleteKnowledgeBase.error && (
              <span className="mt-2 block text-destructive">
                {deleteKnowledgeBase.error.message}
              </span>
            )}
          </>
        }
        isPending={deleteKnowledgeBase.isPending}
        onConfirm={() => {
          void handleDelete()
        }}
        onOpenChange={setShowDelete}
        open={showDelete}
        title="Delete Knowledge Base"
      />
    </>
  )
}
