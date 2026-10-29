import { zodResolver } from "@hookform/resolvers/zod"
import { Plus } from "lucide-react"
import { useState } from "react"
import { Controller, useForm } from "react-hook-form"

import { ErrorBanner } from "@/components/error-banner"
import { Button } from "@/components/ui/button"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Switch } from "@/components/ui/switch"
import { useCreateDataSource } from "@/mutations/bedrockAgent"
import {
  type DataSourceFormData,
  dataSourceFormSchema,
  EMPTY_DATA_SOURCE_FORM_DEFAULTS,
} from "@/types/bedrockAgent"

export function AddDataSourceForm({
  knowledgeBaseId,
}: {
  knowledgeBaseId: string
}) {
  const [open, setOpen] = useState(false)
  const createDataSource = useCreateDataSource()

  const {
    control,
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
        <Plus data-icon="inline-start" />
        Add data source
      </Button>
    )
  }

  return (
    <div className="flex flex-col gap-3 rounded-md border p-3">
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

        <Controller
          control={control}
          name="chunkingEnabled"
          render={({ field }) => (
            <Field>
              <FieldTitle>Chunking</FieldTitle>
              <div className="flex items-center gap-2 text-xs">
                <Switch
                  aria-label="Enable fixed-size chunking"
                  checked={field.value}
                  id="ds-chunking"
                  onCheckedChange={field.onChange}
                />
                <Label htmlFor="ds-chunking">
                  Split documents using fixed-size chunking
                </Label>
              </div>
            </Field>
          )}
        />

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
