import { zodResolver } from "@hookform/resolvers/zod"
import { useNavigate } from "@tanstack/react-router"
import { Plus, Trash2 } from "lucide-react"
import {
  type Control,
  type FieldErrors,
  type Path,
  type UseFormRegister,
  useFieldArray,
  useForm,
} from "react-hook-form"

import { BackLink } from "@/components/back-link"
import { ErrorBanner } from "@/components/error-banner"
import { FormActions } from "@/components/form-actions"
import { SelectField, selectOptions, TextField } from "@/components/form-fields"
import { PageHeading } from "@/components/page-heading"
import { TagsFieldArray } from "@/components/tags-field-array"
import { Button } from "@/components/ui/button"
import {
  Field,
  FieldDescription,
  FieldError,
  FieldLegend,
  FieldSet,
  FieldTitle,
} from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Textarea } from "@/components/ui/textarea"
import { useCreateGuardrail, useUpdateGuardrail } from "@/mutations/bedrock"
import {
  CONTENT_FILTER_TYPES,
  CONTEXTUAL_GROUNDING_FILTER_TYPES,
  FILTER_STRENGTHS,
  type GuardrailFormData,
  guardrailFormSchema,
  PII_ENTITY_TYPES,
  SENSITIVE_INFORMATION_ACTIONS,
} from "@/types/bedrock"

// react-hook-form cannot infer a path built from a runtime array index, so
// every array-row field name is funnelled through this one assertion.
function path(template: string): Path<GuardrailFormData> {
  // oxlint-disable-next-line typescript/no-unsafe-type-assertion -- react-hook-form cannot infer a path built from a runtime array index
  return template as Path<GuardrailFormData>
}

type GuardrailFormPageProps =
  | { mode: "create"; defaultValues: GuardrailFormData }
  | { mode: "edit"; guardrailId: string; defaultValues: GuardrailFormData }

interface SectionProps {
  control: Control<GuardrailFormData>
  register: UseFormRegister<GuardrailFormData>
  errors: FieldErrors<GuardrailFormData>
}

function TopicsFieldArray({ control, register, errors }: SectionProps) {
  const { fields, append, remove } = useFieldArray({
    control,
    name: "topics",
  })

  return (
    <div className="space-y-3">
      {fields.map((field, index) => (
        <div className="space-y-2 rounded-md border p-3" key={field.id}>
          <TextField
            error={errors.topics?.[index]?.name}
            id={`topic-name-${field.id}`}
            label="Topic name"
            {...register(path(`topics.${index}.name`))}
          />
          <Field>
            <FieldTitle>
              <label htmlFor={`topic-definition-${field.id}`}>Definition</label>
            </FieldTitle>
            <Textarea
              aria-invalid={!!errors.topics?.[index]?.definition}
              id={`topic-definition-${field.id}`}
              rows={2}
              {...register(path(`topics.${index}.definition`))}
            />
            <FieldError errors={[errors.topics?.[index]?.definition]} />
          </Field>
          <Field>
            <FieldTitle>
              <label htmlFor={`topic-examples-${field.id}`}>
                Example prompts
              </label>
            </FieldTitle>
            <Textarea
              id={`topic-examples-${field.id}`}
              placeholder="One example prompt per line"
              rows={2}
              {...register(path(`topics.${index}.examplesText`))}
            />
          </Field>
          <Button
            onClick={() => {
              remove(index)
            }}
            size="sm"
            type="button"
            variant="ghost"
          >
            <Trash2 className="size-3.5" />
            Remove topic
          </Button>
        </div>
      ))}
      <Button
        onClick={() => {
          append({ name: "", definition: "", examplesText: "" })
        }}
        size="sm"
        type="button"
        variant="outline"
      >
        <Plus className="size-3.5" />
        Add denied topic
      </Button>
    </div>
  )
}

function ContentFiltersFieldArray({ control }: SectionProps) {
  const { fields, append, remove } = useFieldArray({
    control,
    name: "contentFilters",
  })

  return (
    <div className="space-y-3">
      {fields.map((field, index) => (
        <div
          className="grid grid-cols-1 gap-2 rounded-md border p-3 sm:grid-cols-3"
          key={field.id}
        >
          <SelectField
            control={control}
            id={`filter-type-${field.id}`}
            label="Category"
            name={path(`contentFilters.${index}.type`)}
            options={selectOptions(CONTENT_FILTER_TYPES)}
          />
          <SelectField
            control={control}
            id={`filter-input-${field.id}`}
            label="Input strength"
            name={path(`contentFilters.${index}.inputStrength`)}
            options={selectOptions(FILTER_STRENGTHS)}
          />
          <SelectField
            control={control}
            id={`filter-output-${field.id}`}
            label="Output strength"
            name={path(`contentFilters.${index}.outputStrength`)}
            options={selectOptions(FILTER_STRENGTHS)}
          />
          <div className="sm:col-span-3">
            <Button
              onClick={() => {
                remove(index)
              }}
              size="sm"
              type="button"
              variant="ghost"
            >
              <Trash2 className="size-3.5" />
              Remove content filter
            </Button>
          </div>
        </div>
      ))}
      <Button
        onClick={() => {
          append({
            type: "SEXUAL",
            inputStrength: "NONE",
            outputStrength: "NONE",
          })
        }}
        size="sm"
        type="button"
        variant="outline"
      >
        <Plus className="size-3.5" />
        Add content filter
      </Button>
    </div>
  )
}

function ProfanityFilterField({ register }: SectionProps) {
  return (
    <Field>
      <FieldTitle>Managed word list</FieldTitle>
      <label className="flex items-center gap-2 text-xs">
        <input
          aria-label="Block profanity"
          type="checkbox"
          {...register("profanityFilter")}
        />
        <span>Block profanity using Bedrock&apos;s managed word list</span>
      </label>
    </Field>
  )
}

function WordsFieldArray({ control, register, errors }: SectionProps) {
  const { fields, append, remove } = useFieldArray({ control, name: "words" })

  return (
    <div className="space-y-2">
      {fields.map((field, index) => (
        <div className="flex items-start gap-2" key={field.id}>
          <div className="min-w-0 flex-1">
            <Input
              aria-invalid={!!errors.words?.[index]?.text}
              placeholder="Blocked word or phrase"
              {...register(path(`words.${index}.text`))}
            />
            <FieldError errors={[errors.words?.[index]?.text]} />
          </div>
          <Button
            aria-label={`Remove word ${index + 1}`}
            onClick={() => {
              remove(index)
            }}
            size="icon"
            type="button"
            variant="ghost"
          >
            <Trash2 className="size-3.5" />
          </Button>
        </div>
      ))}
      <Button
        onClick={() => {
          append({ text: "" })
        }}
        size="sm"
        type="button"
        variant="outline"
      >
        <Plus className="size-3.5" />
        Add word
      </Button>
    </div>
  )
}

function PiiEntitiesFieldArray({ control }: SectionProps) {
  const { fields, append, remove } = useFieldArray({
    control,
    name: "piiEntities",
  })

  return (
    <div className="space-y-3">
      {fields.map((field, index) => (
        <div
          className="grid grid-cols-1 gap-2 rounded-md border p-3 sm:grid-cols-2"
          key={field.id}
        >
          <SelectField
            control={control}
            id={`pii-type-${field.id}`}
            label="PII type"
            name={path(`piiEntities.${index}.type`)}
            options={selectOptions(PII_ENTITY_TYPES)}
          />
          <SelectField
            control={control}
            id={`pii-action-${field.id}`}
            label="Action"
            name={path(`piiEntities.${index}.action`)}
            options={selectOptions(SENSITIVE_INFORMATION_ACTIONS)}
          />
          <div className="sm:col-span-2">
            <Button
              onClick={() => {
                remove(index)
              }}
              size="sm"
              type="button"
              variant="ghost"
            >
              <Trash2 className="size-3.5" />
              Remove PII entity
            </Button>
          </div>
        </div>
      ))}
      <Button
        onClick={() => {
          append({ type: "EMAIL", action: "BLOCK" })
        }}
        size="sm"
        type="button"
        variant="outline"
      >
        <Plus className="size-3.5" />
        Add PII entity
      </Button>
    </div>
  )
}

function RegexesFieldArray({ control, register, errors }: SectionProps) {
  const { fields, append, remove } = useFieldArray({
    control,
    name: "regexes",
  })

  return (
    <div className="space-y-3">
      {fields.map((field, index) => (
        <div className="space-y-2 rounded-md border p-3" key={field.id}>
          <TextField
            error={errors.regexes?.[index]?.name}
            id={`regex-name-${field.id}`}
            label="Name"
            {...register(path(`regexes.${index}.name`))}
          />
          <TextField
            error={errors.regexes?.[index]?.pattern}
            id={`regex-pattern-${field.id}`}
            label="Pattern"
            {...register(path(`regexes.${index}.pattern`))}
          />
          <TextField
            id={`regex-description-${field.id}`}
            label="Description"
            placeholder="Optional"
            {...register(path(`regexes.${index}.description`))}
          />
          <SelectField
            control={control}
            id={`regex-action-${field.id}`}
            label="Action"
            name={path(`regexes.${index}.action`)}
            options={selectOptions(SENSITIVE_INFORMATION_ACTIONS)}
          />
          <Button
            onClick={() => {
              remove(index)
            }}
            size="sm"
            type="button"
            variant="ghost"
          >
            <Trash2 className="size-3.5" />
            Remove regex
          </Button>
        </div>
      ))}
      <Button
        onClick={() => {
          append({ name: "", pattern: "", action: "BLOCK", description: "" })
        }}
        size="sm"
        type="button"
        variant="outline"
      >
        <Plus className="size-3.5" />
        Add regex
      </Button>
    </div>
  )
}

const DEFAULT_GROUNDING_THRESHOLD = 0.5

function ContextualGroundingFiltersFieldArray({
  control,
  register,
  errors,
}: SectionProps) {
  const { fields, append, remove } = useFieldArray({
    control,
    name: "contextualGroundingFilters",
  })

  return (
    <div className="space-y-3">
      {fields.map((field, index) => (
        <div
          className="grid grid-cols-1 gap-2 rounded-md border p-3 sm:grid-cols-2"
          key={field.id}
        >
          <SelectField
            control={control}
            id={`grounding-type-${field.id}`}
            label="Filter type"
            name={path(`contextualGroundingFilters.${index}.type`)}
            options={selectOptions(CONTEXTUAL_GROUNDING_FILTER_TYPES)}
          />
          <TextField
            error={errors.contextualGroundingFilters?.[index]?.threshold}
            id={`grounding-threshold-${field.id}`}
            label="Threshold"
            max={1}
            min={0}
            step={0.01}
            type="number"
            {...register(
              path(`contextualGroundingFilters.${index}.threshold`),
              { valueAsNumber: true },
            )}
          />
          <div className="sm:col-span-2">
            <Button
              onClick={() => {
                remove(index)
              }}
              size="sm"
              type="button"
              variant="ghost"
            >
              <Trash2 className="size-3.5" />
              Remove filter
            </Button>
          </div>
        </div>
      ))}
      <Button
        onClick={() => {
          append({ type: "GROUNDING", threshold: DEFAULT_GROUNDING_THRESHOLD })
        }}
        size="sm"
        type="button"
        variant="outline"
      >
        <Plus className="size-3.5" />
        Add contextual grounding filter
      </Button>
    </div>
  )
}

export function GuardrailFormPage(props: GuardrailFormPageProps) {
  const { mode, defaultValues } = props
  const navigate = useNavigate()
  const createGuardrail = useCreateGuardrail()
  const updateGuardrail = useUpdateGuardrail()

  const {
    control,
    formState: { errors, isSubmitting },
    handleSubmit,
    register,
  } = useForm<GuardrailFormData>({
    resolver: zodResolver(guardrailFormSchema),
    defaultValues,
  })

  const onSubmit = async (data: GuardrailFormData) => {
    if (mode === "create") {
      const result = await createGuardrail.mutateAsync(data)
      await navigate({
        params: { guardrailId: result.guardrailId ?? "" },
        to: "/bedrock/list-guardrails/$guardrailId",
      })

      return
    }

    await updateGuardrail.mutateAsync({
      data,
      guardrailIdentifier: props.guardrailId,
    })
    await navigate({
      params: { guardrailId: props.guardrailId },
      to: "/bedrock/list-guardrails/$guardrailId",
    })
  }

  const handleCancel = async () => {
    if (mode === "edit") {
      await navigate({
        params: { guardrailId: props.guardrailId },
        to: "/bedrock/list-guardrails/$guardrailId",
      })

      return
    }

    await navigate({ to: "/bedrock/list-guardrails" })
  }

  const mutation = mode === "create" ? createGuardrail : updateGuardrail
  const sectionProps: SectionProps = { control, errors, register }

  return (
    <>
      {mode === "edit" ? (
        <BackLink
          params={{ guardrailId: props.guardrailId }}
          to="/bedrock/list-guardrails/$guardrailId"
        >
          Back to guardrail
        </BackLink>
      ) : (
        <BackLink to="/bedrock/list-guardrails">Back to guardrails</BackLink>
      )}
      <PageHeading
        title={mode === "create" ? "Create Guardrail" : "Edit Guardrail"}
      />

      {mutation.error && (
        <ErrorBanner
          error={mutation.error}
          msg={`Failed to ${mode === "create" ? "create" : "update"} the guardrail`}
        />
      )}

      <form className="max-w-4xl space-y-6" onSubmit={handleSubmit(onSubmit)}>
        <TextField
          description="Letters, digits, hyphens and underscores only."
          error={errors.name}
          id="guardrail-name"
          label="Name"
          placeholder="content-safety"
          {...register("name")}
        />

        <Field>
          <FieldTitle>
            <label htmlFor="guardrail-description">Description</label>
          </FieldTitle>
          <Textarea
            id="guardrail-description"
            placeholder="Optional"
            rows={2}
            {...register("description")}
          />
          <FieldError errors={[errors.description]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="guardrail-blocked-input">
              Blocked input message
            </label>
          </FieldTitle>
          <Textarea
            aria-invalid={!!errors.blockedInputMessaging}
            id="guardrail-blocked-input"
            rows={2}
            {...register("blockedInputMessaging")}
          />
          <FieldDescription>
            Shown when the guardrail blocks a prompt.
          </FieldDescription>
          <FieldError errors={[errors.blockedInputMessaging]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="guardrail-blocked-output">
              Blocked output message
            </label>
          </FieldTitle>
          <Textarea
            aria-invalid={!!errors.blockedOutputsMessaging}
            id="guardrail-blocked-output"
            rows={2}
            {...register("blockedOutputsMessaging")}
          />
          <FieldDescription>
            Shown when the guardrail blocks a model response.
          </FieldDescription>
          <FieldError errors={[errors.blockedOutputsMessaging]} />
        </Field>

        <TextField
          description="Leave blank to use the account default."
          id="guardrail-kms-key"
          label="KMS key ARN"
          placeholder="Optional"
          {...register("kmsKeyId")}
        />

        <FieldSet>
          <FieldLegend>Denied topics</FieldLegend>
          <TopicsFieldArray {...sectionProps} />
        </FieldSet>

        <FieldSet>
          <FieldLegend>Content filters</FieldLegend>
          <ContentFiltersFieldArray {...sectionProps} />
        </FieldSet>

        <FieldSet>
          <FieldLegend>Word filters</FieldLegend>
          <ProfanityFilterField {...sectionProps} />
          <WordsFieldArray {...sectionProps} />
        </FieldSet>

        <FieldSet>
          <FieldLegend>Sensitive information</FieldLegend>
          <PiiEntitiesFieldArray {...sectionProps} />
          <RegexesFieldArray {...sectionProps} />
        </FieldSet>

        <FieldSet>
          <FieldLegend>Contextual grounding</FieldLegend>
          <ContextualGroundingFiltersFieldArray {...sectionProps} />
        </FieldSet>

        {mode === "create" && <TagsFieldArray control={control} name="tags" />}

        <FormActions
          isPending={mutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={() => {
            void handleCancel()
          }}
          pendingLabel={mode === "create" ? "Creating…" : "Saving…"}
          submitLabel={mode === "create" ? "Create Guardrail" : "Save Changes"}
        />
      </form>
    </>
  )
}
