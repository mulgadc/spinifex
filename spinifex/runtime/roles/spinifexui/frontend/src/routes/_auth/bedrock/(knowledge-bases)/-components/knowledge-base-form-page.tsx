import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery } from "@tanstack/react-query"
import {
  Controller,
  type DeepPartialSkipArrayKey,
  useForm,
  useWatch,
} from "react-hook-form"

import {
  type CliCommand,
  CliCommandPanel,
  shellSingleQuote,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { FormLock } from "@/components/form-lock"
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
  formToCreateKnowledgeBaseInput,
  kbFormSchema,
  type KnowledgeBaseFormData,
} from "@/types/bedrockAgent"

export function KnowledgeBaseFormPage({
  onClose,
}: {
  onClose: (knowledgeBaseId?: string) => void
}) {
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

  const values = useWatch({ control })

  const onSubmit = async (data: KnowledgeBaseFormData) => {
    try {
      const result = await createKnowledgeBase.mutateAsync(data)

      onClose(result.knowledgeBase?.knowledgeBaseId)
    } catch {
      // Surfaced beside the submit button through createKnowledgeBase.error.
    }
  }

  return (
    <form onSubmit={handleSubmit(onSubmit)}>
      <FormLock
        className="space-y-6"
        locked={createKnowledgeBase.isPending || isSubmitting}
      >
        <FieldDescription>
          After the knowledge base is created, add one or more S3 data sources
          to it from its detail page, then run an ingestion job to load them.
        </FieldDescription>

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

        <CliCommandPanel commands={buildCreateKnowledgeBaseCommands(values)} />

        <FormActions
          error={createKnowledgeBase.error}
          errorMsg="Failed to create the knowledge base"
          isPending={createKnowledgeBase.isPending}
          isSubmitting={isSubmitting}
          onCancel={() => {
            onClose()
          }}
          pendingLabel="Creating…"
          submitLabel="Create Knowledge Base"
        />
      </FormLock>
    </form>
  )
}

// The configuration nests too deeply for flags, so the request goes as one
// JSON document, the same input the console sends.
function buildCreateKnowledgeBaseCommands(
  values: DeepPartialSkipArrayKey<KnowledgeBaseFormData>,
): CliCommand[] {
  const input = formToCreateKnowledgeBaseInput({
    ...EMPTY_KB_FORM_DEFAULTS,
    ...values,
    name: values.name || "<Name>",
    embeddingModelArn: values.embeddingModelArn || "<EmbeddingModelArn>",
  })

  return [
    {
      label: "Create knowledge base",
      parts: [
        {
          type: "bin",
          value: "AWS_PROFILE=spinifex aws bedrock-agent create-knowledge-base",
        },
        { type: "flag", value: " --cli-input-json" },
        { type: "value", value: ` ${shellSingleQuote(JSON.stringify(input))}` },
      ],
    },
  ]
}
