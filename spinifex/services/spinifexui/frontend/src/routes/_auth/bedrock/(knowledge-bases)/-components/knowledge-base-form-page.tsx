import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery } from "@tanstack/react-query"
import { useNavigate } from "@tanstack/react-router"
import { Controller, useForm } from "react-hook-form"

import { BackLink } from "@/components/back-link"
import { ErrorBanner } from "@/components/error-banner"
import { FormActions } from "@/components/form-actions"
import { PageHeading } from "@/components/page-heading"
import {
  Field,
  FieldDescription,
  FieldError,
  FieldTitle,
} from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Textarea } from "@/components/ui/textarea"
import { useCreateKnowledgeBase } from "@/mutations/bedrockAgent"
import { foundationModelsQueryOptions } from "@/queries/bedrock"
import {
  dimensionsForModelArn,
  EMPTY_KB_FORM_DEFAULTS,
  kbFormSchema,
  type KnowledgeBaseFormData,
} from "@/types/bedrockAgent"

export function KnowledgeBaseFormPage() {
  const navigate = useNavigate()
  const createKnowledgeBase = useCreateKnowledgeBase()
  const { data: modelsData } = useSuspenseQuery(foundationModelsQueryOptions)

  const embeddingModels = (modelsData.modelSummaries ?? []).filter((model) =>
    model.outputModalities?.includes("EMBEDDING"),
  )

  const {
    control,
    formState: { errors, isSubmitting },
    handleSubmit,
    register,
    setValue,
  } = useForm<KnowledgeBaseFormData>({
    resolver: zodResolver(kbFormSchema),
    defaultValues: EMPTY_KB_FORM_DEFAULTS,
  })

  const onSubmit = async (data: KnowledgeBaseFormData) => {
    const result = await createKnowledgeBase.mutateAsync(data)
    await navigate({
      params: {
        knowledgeBaseId: result.knowledgeBase?.knowledgeBaseId ?? "",
      },
      to: "/bedrock/list-knowledge-bases/$knowledgeBaseId",
    })
  }

  const handleCancel = async () => {
    await navigate({ to: "/bedrock/list-knowledge-bases" })
  }

  return (
    <>
      <BackLink to="/bedrock/list-knowledge-bases">
        Back to knowledge bases
      </BackLink>
      <PageHeading title="Create Knowledge Base" />

      {createKnowledgeBase.error && (
        <ErrorBanner
          error={createKnowledgeBase.error}
          msg="Failed to create the knowledge base"
        />
      )}

      <form className="max-w-4xl space-y-6" onSubmit={handleSubmit(onSubmit)}>
        <Field>
          <FieldTitle>
            <label htmlFor="kb-name">Name</label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors.name}
            id="kb-name"
            placeholder="product-docs"
            {...register("name")}
          />
          <FieldError errors={[errors.name]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="kb-description">Description</label>
          </FieldTitle>
          <Textarea
            id="kb-description"
            placeholder="Optional"
            rows={2}
            {...register("description")}
          />
          <FieldError errors={[errors.description]} />
        </Field>

        <Controller
          control={control}
          name="embeddingModelArn"
          render={({ field, fieldState }) => (
            <Field>
              <FieldTitle>
                <label htmlFor="kb-embedding-model">Embedding model</label>
              </FieldTitle>
              <Select
                onValueChange={(value) => {
                  field.onChange(value ?? "")
                  const known = dimensionsForModelArn(value ?? "")
                  if (known) {
                    setValue("dimensions", known)
                  }
                }}
                value={field.value}
              >
                <SelectTrigger
                  aria-invalid={!!fieldState.error}
                  className="w-full"
                  id="kb-embedding-model"
                >
                  <SelectValue placeholder="Select an embedding model" />
                </SelectTrigger>
                <SelectContent>
                  {embeddingModels.map((model) => (
                    <SelectItem
                      key={model.modelArn}
                      value={model.modelArn ?? ""}
                    >
                      {model.modelName ?? model.modelId}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <FieldError errors={[fieldState.error]} />
            </Field>
          )}
        />

        <Field>
          <FieldTitle>
            <label htmlFor="kb-dimensions">Dimensions</label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors.dimensions}
            id="kb-dimensions"
            min={1}
            type="number"
            {...register("dimensions", { valueAsNumber: true })}
          />
          <FieldDescription>
            Vector size the chosen embedding model produces.
          </FieldDescription>
          <FieldError errors={[errors.dimensions]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="kb-role-arn">Role ARN</label>
          </FieldTitle>
          <Input
            id="kb-role-arn"
            placeholder="Optional"
            {...register("roleArn")}
          />
          <FieldDescription>
            Stored and echoed back only; leave blank to use a default.
          </FieldDescription>
          <FieldError errors={[errors.roleArn]} />
        </Field>

        <FormActions
          isPending={createKnowledgeBase.isPending}
          isSubmitting={isSubmitting}
          onCancel={() => {
            void handleCancel()
          }}
          pendingLabel="Creating…"
          submitLabel="Create Knowledge Base"
        />
      </form>
    </>
  )
}
