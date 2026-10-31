import type {
  Rule,
  RuleCondition,
  TargetGroup,
} from "@aws-sdk/client-elastic-load-balancing-v2"
import { zodResolver } from "@hookform/resolvers/zod"
import { Trash2 } from "lucide-react"
import type { SubmitEvent } from "react"
import {
  type DeepPartial,
  Controller,
  useFieldArray,
  useForm,
  useWatch,
} from "react-hook-form"

import {
  type CliCommand,
  CliCommandPanel,
  cliPlaceholder,
  commandFlag,
  shellSingleQuote,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { FormLock } from "@/components/form-lock"
import { FormPanel } from "@/components/form-panel"
import { Button } from "@/components/ui/button"
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
import { type RuleCondInput, toSDKConditions } from "@/mutations/elbv2"
import {
  type CreateRuleFormData,
  type RuleConditionField,
  type RuleConditionFormData,
  RULE_CONDITION_FIELDS,
  createRuleSchema,
} from "@/types/elbv2"

function formatHttpHeaderValues(c: RuleCondition): string[] {
  const name = c.HttpHeaderConfig?.HttpHeaderName ?? "?"
  const vals = c.HttpHeaderConfig?.Values ?? []

  return vals.map((v) => `${name}=${v}`)
}

function formatQueryStringValues(c: RuleCondition): string[] {
  return (c.QueryStringConfig?.Values ?? []).map((kv) =>
    kv.Key ? `${kv.Key}=${kv.Value ?? ""}` : (kv.Value ?? ""),
  )
}

export function collectConditionValues(c: RuleCondition): string[] {
  if (c.Field === undefined) {
    return c.Values ?? []
  }

  switch (c.Field) {
    case "host-header": {
      return c.HostHeaderConfig?.Values ?? c.Values ?? []
    }

    case "path-pattern": {
      return c.PathPatternConfig?.Values ?? c.Values ?? []
    }

    case "http-header": {
      return formatHttpHeaderValues(c)
    }

    case "http-request-method": {
      return c.HttpRequestMethodConfig?.Values ?? []
    }

    case "source-ip": {
      return c.SourceIpConfig?.Values ?? []
    }

    case "query-string": {
      return formatQueryStringValues(c)
    }

    default: {
      return c.Values ?? []
    }
  }
}

export function toRuleCondInput(c: RuleConditionFormData): RuleCondInput {
  const values = c.rawValues
    .split(/[\n,]/)
    .map((v) => v.trim())
    .filter((v) => v.length > 0)

  if (c.field === "query-string") {
    return {
      field: c.field,
      values: [],
      queryStringPairs: values.map((v) => {
        const eq = v.indexOf("=")

        if (eq === -1) {
          return { value: v }
        }

        return { key: v.slice(0, eq), value: v.slice(eq + 1) }
      }),
    }
  }

  return {
    field: c.field,
    values,
    httpHeaderName: c.httpHeaderName,
  }
}

// A new rule starts at the highest priority with a single empty path match.
const DEFAULT_PRIORITY = 1

const defaultConditions: RuleConditionFormData[] = [
  { field: "path-pattern", rawValues: "" },
]

// Projects an existing rule onto the form shape, falling back to the defaults
// for a non-numeric priority or a rule that carries no conditions.
function ruleFormValues(rule: Rule): CreateRuleFormData {
  const priority = Number(rule.Priority ?? DEFAULT_PRIORITY)

  const conditions: RuleConditionFormData[] =
    rule.Conditions?.map((c) => fromSDKCondition(c)) ?? []

  return {
    priority: Number.isFinite(priority) ? priority : DEFAULT_PRIORITY,
    conditions: conditions.length > 0 ? conditions : defaultConditions,
    forwardTargetGroupArn: rule.Actions?.[0]?.TargetGroupArn ?? "",
  }
}

interface ListenerRuleFormProps {
  listenerArn: string
  onSubmit: (data: CreateRuleFormData) => Promise<void>
  onClose: () => void
  isPending: boolean
  error: Error | null
  targetGroups: TargetGroup[]
  // Set to edit that rule; unset adds a new one.
  rule?: Rule
}

export function ListenerRuleForm({
  listenerArn,
  onSubmit,
  onClose,
  isPending,
  error,
  targetGroups,
  rule,
}: ListenerRuleFormProps) {
  const editing = rule !== undefined

  const form = useForm<CreateRuleFormData>({
    resolver: zodResolver(createRuleSchema),
    defaultValues: rule
      ? ruleFormValues(rule)
      : {
          priority: DEFAULT_PRIORITY,
          conditions: defaultConditions,
          forwardTargetGroupArn: "",
        },
  })

  const values = useWatch({ control: form.control })

  const { fields, append, remove } = useFieldArray({
    control: form.control,
    name: "conditions",
  })

  function handleSubmit(event: SubmitEvent<HTMLFormElement>) {
    event.stopPropagation()
    void form.handleSubmit(onSubmit)(event)
  }

  return (
    <FormPanel
      onClose={onClose}
      subtitle={rule ? `Priority ${rule.Priority}` : undefined}
      title={editing ? "Edit rule" : "Add rule"}
    >
      <form onSubmit={handleSubmit}>
        <FormLock className="space-y-4" locked={isPending}>
          <FieldDescription>
            Routing rules match incoming requests and forward them to a target
            group. Rules evaluate in priority order; the listener default
            applies when no rule matches.
          </FieldDescription>
          <Field>
            <FieldTitle>
              <label htmlFor="rule-priority">Priority</label>
            </FieldTitle>
            <Input
              aria-invalid={!!form.formState.errors.priority}
              id="rule-priority"
              inputMode="numeric"
              type="number"
              {...form.register("priority", { valueAsNumber: true })}
            />
            <FieldError errors={[form.formState.errors.priority]} />
          </Field>

          <div className="space-y-3">
            <div className="flex items-center justify-between">
              <FieldTitle>Conditions</FieldTitle>
              <Button
                disabled={fields.length >= 5}
                onClick={() => {
                  append({ field: "path-pattern", rawValues: "" })
                }}
                size="sm"
                type="button"
                variant="outline"
              >
                Add condition
              </Button>
            </div>
            {fields.map((field, idx) => (
              <ConditionRow
                form={form}
                index={idx}
                key={field.id}
                onRemove={
                  fields.length > 1
                    ? () => {
                        remove(idx)
                      }
                    : undefined
                }
              />
            ))}
          </div>

          <Field>
            <FieldTitle>
              <label htmlFor="rule-forward-tg">Forward to target group</label>
            </FieldTitle>
            <Controller
              control={form.control}
              name="forwardTargetGroupArn"
              render={({ field }) => (
                <Select
                  onValueChange={field.onChange}
                  value={field.value ?? ""}
                >
                  <SelectTrigger
                    aria-invalid={!!form.formState.errors.forwardTargetGroupArn}
                    className="w-full"
                    id="rule-forward-tg"
                  >
                    <SelectValue placeholder="Select target group" />
                  </SelectTrigger>
                  <SelectContent>
                    {targetGroups.map((tg) => (
                      <SelectItem
                        key={tg.TargetGroupArn}
                        value={tg.TargetGroupArn ?? ""}
                      >
                        {tg.TargetGroupName} · {tg.Protocol}:{tg.Port}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              )}
            />
            <FieldError
              errors={[form.formState.errors.forwardTargetGroupArn]}
            />
          </Field>
          <CliCommandPanel
            commands={buildRuleCommands(
              values,
              rule?.RuleArn ?? listenerArn,
              editing,
            )}
          />
          <FormActions
            error={error}
            errorMsg={
              editing ? "Failed to modify rule" : "Failed to create rule"
            }
            isPending={isPending}
            isSubmitting={form.formState.isSubmitting}
            onCancel={onClose}
            pendingLabel={editing ? "Saving…" : "Adding…"}
            submitLabel={editing ? "Save changes" : "Add rule"}
          />
        </FormLock>
      </form>
    </FormPanel>
  )
}

// The conditions go as JSON in the shape the console sends them.
export function buildRuleCommands(
  values: DeepPartial<CreateRuleFormData>,
  arn: string,
  editing: boolean,
): CliCommand[] {
  const conditions = toSDKConditions(
    (values.conditions ?? []).map((c) =>
      toRuleCondInput({
        field: c?.field ?? "path-pattern",
        rawValues: c?.rawValues ?? "",
        httpHeaderName: c?.httpHeaderName,
      }),
    ),
  )

  const target = cliPlaceholder(
    values.forwardTargetGroupArn ?? "",
    "TargetGroupArn",
  )

  return [
    {
      label: editing ? "Modify rule" : "Create rule",
      parts: [
        {
          type: "bin",
          value: `AWS_PROFILE=spinifex aws elbv2 ${editing ? "modify-rule" : "create-rule"}`,
        },
        ...commandFlag(editing ? "--rule-arn" : "--listener-arn", arn),
        ...(editing
          ? []
          : commandFlag("--priority", values.priority ?? DEFAULT_PRIORITY)),
        ...commandFlag(
          "--conditions",
          shellSingleQuote(JSON.stringify(conditions)),
        ),
        ...commandFlag("--actions", `Type=forward,TargetGroupArn=${target}`),
      ],
    },
  ]
}

const CONDITION_FIELD_TEXT: Record<
  RuleConditionField,
  { label: string; placeholder: string }
> = {
  "path-pattern": { label: "Path pattern", placeholder: "/api/*" },
  "host-header": { label: "Host header", placeholder: "*.example.com" },
  "http-header": { label: "HTTP header", placeholder: "acme" },
  "http-request-method": { label: "HTTP method", placeholder: "GET, POST" },
  "source-ip": { label: "Source IP CIDR", placeholder: "10.0.0.0/8" },
  "query-string": { label: "Query string", placeholder: "key=value" },
}

interface ConditionRowProps {
  form: ReturnType<typeof useForm<CreateRuleFormData>>
  index: number
  onRemove?: () => void
}

function ConditionRow({ form, index, onRemove }: ConditionRowProps) {
  const fieldName = form.watch(`conditions.${index}.field`)
  const errors = form.formState.errors.conditions?.[index]

  return (
    <div className="space-y-2 rounded-md border p-3">
      <div className="flex gap-2">
        <div className="flex-1">
          <FieldTitle>
            <label htmlFor={`rule-cond-${index}-field`}>Type</label>
          </FieldTitle>
          <Controller
            control={form.control}
            name={`conditions.${index}.field`}
            render={({ field }) => (
              <Select onValueChange={field.onChange} value={field.value}>
                <SelectTrigger
                  className="w-full"
                  id={`rule-cond-${index}-field`}
                >
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {RULE_CONDITION_FIELDS.map((f) => (
                    <SelectItem key={f} value={f}>
                      {CONDITION_FIELD_TEXT[f].label}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}
          />
        </div>
        {onRemove && (
          <Button
            aria-label={`Remove condition ${index + 1}`}
            className="self-end"
            onClick={onRemove}
            size="sm"
            type="button"
            variant="ghost"
          >
            <Trash2 className="size-4" />
          </Button>
        )}
      </div>

      {fieldName === "http-header" && (
        <Field>
          <FieldTitle>
            <label htmlFor={`rule-cond-${index}-hdr-name`}>Header name</label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors?.httpHeaderName}
            id={`rule-cond-${index}-hdr-name`}
            placeholder="X-Tenant"
            {...form.register(`conditions.${index}.httpHeaderName`)}
          />
          <FieldError errors={[errors?.httpHeaderName]} />
        </Field>
      )}

      <Field>
        <FieldTitle>
          <label htmlFor={`rule-cond-${index}-values`}>
            Values{" "}
            <span className="text-xs text-muted-foreground">
              (comma or newline separated, max 5)
            </span>
          </label>
        </FieldTitle>
        <Input
          aria-invalid={!!errors?.rawValues}
          id={`rule-cond-${index}-values`}
          placeholder={CONDITION_FIELD_TEXT[fieldName].placeholder}
          {...form.register(`conditions.${index}.rawValues`)}
        />
        <FieldError errors={[errors?.rawValues]} />
      </Field>
    </div>
  )
}

function isSupportedField(
  value: string | undefined,
): value is RuleConditionField {
  return value !== undefined && Object.hasOwn(CONDITION_FIELD_TEXT, value)
}

function fromSDKCondition(c: RuleCondition): RuleConditionFormData {
  const values = collectConditionValues(c)
  const field = isSupportedField(c.Field) ? c.Field : "path-pattern"

  return {
    field,
    httpHeaderName: c.HttpHeaderConfig?.HttpHeaderName,
    rawValues: values.join(", "),
  }
}
