import type {
  Rule,
  RuleCondition,
  TargetGroup,
} from "@aws-sdk/client-elastic-load-balancing-v2"
import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Pencil, Trash2 } from "lucide-react"
import { useEffect, useState } from "react"
import { Controller, useFieldArray, useForm } from "react-hook-form"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { ErrorBanner } from "@/components/error-banner"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Button } from "@/components/ui/button"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import {
  type RuleCondInput,
  useCreateRule,
  useDeleteRule,
  useModifyRule,
} from "@/mutations/elbv2"
import { elbv2ListenerRulesQueryOptions } from "@/queries/elbv2"
import {
  type CreateRuleFormData,
  type RuleConditionFormData,
  createRuleSchema,
} from "@/types/elbv2"

interface ListenerRulesTabProps {
  listenerArn: string
  targetGroups: TargetGroup[]
}

export function ListenerRulesTab({
  listenerArn,
  targetGroups,
}: ListenerRulesTabProps) {
  const { data: rulesData } = useSuspenseQuery(
    elbv2ListenerRulesQueryOptions(listenerArn),
  )
  const createMutation = useCreateRule()
  const modifyMutation = useModifyRule()
  const deleteMutation = useDeleteRule()

  const [addOpen, setAddOpen] = useState(false)
  const [editTarget, setEditTarget] = useState<Rule | undefined>()
  const [deleteTarget, setDeleteTarget] = useState<Rule | undefined>()

  const rules = rulesData.Rules ?? []

  const tgNameByArn = new Map<string, string>()
  for (const tg of targetGroups) {
    if (tg.TargetGroupArn && tg.TargetGroupName) {
      tgNameByArn.set(tg.TargetGroupArn, tg.TargetGroupName)
    }
  }

  const handleCreate = async (data: CreateRuleFormData) => {
    try {
      await createMutation.mutateAsync({
        listenerArn,
        priority: data.priority,
        conditions: data.conditions.map(toRuleCondInput),
        forwardTargetGroupArn: data.forwardTargetGroupArn,
      })
      setAddOpen(false)
    } catch {
      // surfaced via mutation.error
    }
  }

  const handleEdit = async (data: CreateRuleFormData) => {
    if (!editTarget?.RuleArn) {
      return
    }
    try {
      await modifyMutation.mutateAsync({
        ruleArn: editTarget.RuleArn,
        listenerArn,
        conditions: data.conditions.map(toRuleCondInput),
        forwardTargetGroupArn: data.forwardTargetGroupArn,
      })
      setEditTarget(undefined)
    } catch {
      // surfaced via mutation.error
    }
  }

  const handleDelete = async () => {
    if (!deleteTarget?.RuleArn) {
      return
    }
    try {
      await deleteMutation.mutateAsync({
        ruleArn: deleteTarget.RuleArn,
        listenerArn,
      })
    } finally {
      setDeleteTarget(undefined)
    }
  }

  return (
    <div className="space-y-3">
      {createMutation.error && (
        <ErrorBanner error={createMutation.error} msg="Failed to create rule" />
      )}
      {modifyMutation.error && (
        <ErrorBanner error={modifyMutation.error} msg="Failed to modify rule" />
      )}
      {deleteMutation.error && (
        <ErrorBanner error={deleteMutation.error} msg="Failed to delete rule" />
      )}

      <div className="flex items-center justify-between">
        <p className="text-xs text-muted-foreground">
          {rules.length} rule{rules.length === 1 ? "" : "s"} (including default)
        </p>
        <Button onClick={() => setAddOpen(true)} size="sm">
          Add rule
        </Button>
      </div>

      <div className="overflow-x-auto rounded-lg border bg-card">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b text-left text-muted-foreground">
              <th className="px-4 py-2 font-medium">Priority</th>
              <th className="px-4 py-2 font-medium">Conditions</th>
              <th className="px-4 py-2 font-medium">Forward to</th>
              <th className="px-4 py-2 font-medium">
                <span className="sr-only">Actions</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {rules.map((rule) => (
              <tr
                className="border-b last:border-0"
                key={rule.RuleArn ?? rule.Priority ?? "default"}
              >
                <td className="px-4 py-2">{rule.Priority}</td>
                <td className="px-4 py-2 text-xs">
                  {formatConditions(rule.Conditions, rule.IsDefault === true)}
                </td>
                <td className="px-4 py-2 text-xs">
                  {formatActions(rule, tgNameByArn)}
                </td>
                <td className="px-4 py-2 text-right">
                  {rule.IsDefault === true ? (
                    <span className="text-xs text-muted-foreground">
                      Edit via listener
                    </span>
                  ) : (
                    <div className="flex justify-end gap-1">
                      <Button
                        aria-label={`Edit rule ${rule.Priority}`}
                        onClick={() => setEditTarget(rule)}
                        size="sm"
                        variant="ghost"
                      >
                        <Pencil className="size-4" />
                      </Button>
                      <Button
                        aria-label={`Delete rule ${rule.Priority}`}
                        onClick={() => setDeleteTarget(rule)}
                        size="sm"
                        variant="ghost"
                      >
                        <Trash2 className="size-4" />
                      </Button>
                    </div>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      <RuleDialog
        isPending={createMutation.isPending}
        mode="add"
        onOpenChange={setAddOpen}
        onSubmit={handleCreate}
        open={addOpen}
        targetGroups={targetGroups}
      />

      <RuleDialog
        initialRule={editTarget}
        isPending={modifyMutation.isPending}
        mode="edit"
        onOpenChange={(open) => !open && setEditTarget(undefined)}
        onSubmit={handleEdit}
        open={editTarget !== undefined}
        targetGroups={targetGroups}
      />

      <DeleteConfirmationDialog
        description={
          deleteTarget
            ? `Delete rule with priority ${deleteTarget.Priority}? This cannot be undone.`
            : ""
        }
        isPending={deleteMutation.isPending}
        onConfirm={handleDelete}
        onOpenChange={(open) => !open && setDeleteTarget(undefined)}
        open={deleteTarget !== undefined}
        title="Delete rule"
      />
    </div>
  )
}

function formatConditions(
  conditions: RuleCondition[] | undefined,
  isDefault: boolean,
): string {
  if (isDefault) {
    return "default"
  }
  if (!conditions || conditions.length === 0) {
    return "—"
  }
  return conditions
    .map((c) => {
      const values = collectConditionValues(c)
      return `${c.Field}: ${values.join(", ")}`
    })
    .join(" AND ")
}

function collectConditionValues(c: RuleCondition): string[] {
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
      const name = c.HttpHeaderConfig?.HttpHeaderName ?? "?"
      const vals = c.HttpHeaderConfig?.Values ?? []
      return vals.map((v) => `${name}=${v}`)
    }
    case "http-request-method": {
      return c.HttpRequestMethodConfig?.Values ?? []
    }
    case "source-ip": {
      return c.SourceIpConfig?.Values ?? []
    }
    case "query-string": {
      return (c.QueryStringConfig?.Values ?? []).map((kv) =>
        kv.Key ? `${kv.Key}=${kv.Value ?? ""}` : (kv.Value ?? ""),
      )
    }
    default: {
      return c.Values ?? []
    }
  }
}

function formatActions(rule: Rule, tgNameByArn: Map<string, string>): string {
  const action = rule.Actions?.[0]
  if (!action) {
    return "—"
  }
  if (action.Type === "forward" && action.TargetGroupArn) {
    return tgNameByArn.get(action.TargetGroupArn) ?? action.TargetGroupArn
  }
  return action.Type ?? "—"
}

function toRuleCondInput(c: RuleConditionFormData): RuleCondInput {
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

interface RuleDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  onSubmit: (data: CreateRuleFormData) => void
  isPending: boolean
  targetGroups: TargetGroup[]
  mode: "add" | "edit"
  initialRule?: Rule
}

function RuleDialog({
  open,
  onOpenChange,
  onSubmit,
  isPending,
  targetGroups,
  mode,
  initialRule,
}: RuleDialogProps) {
  const defaultConds: RuleConditionFormData[] = [
    { field: "path-pattern", rawValues: "" },
  ]
  const form = useForm<CreateRuleFormData>({
    resolver: zodResolver(createRuleSchema),
    defaultValues: {
      priority: 1,
      conditions: defaultConds,
      forwardTargetGroupArn: "",
    },
  })
  const { fields, append, remove } = useFieldArray({
    control: form.control,
    name: "conditions",
  })

  useEffect(() => {
    if (mode === "add") {
      if (!open) {
        form.reset({
          priority: 1,
          conditions: defaultConds,
          forwardTargetGroupArn: "",
        })
      }
      return
    }
    if (!initialRule) {
      return
    }
    const priorityNum = Number(initialRule.Priority ?? 1)
    const conditions: RuleConditionFormData[] =
      initialRule.Conditions?.map((c) => fromSDKCondition(c)) ?? defaultConds
    form.reset({
      priority: Number.isFinite(priorityNum) ? priorityNum : 1,
      conditions: conditions.length > 0 ? conditions : defaultConds,
      forwardTargetGroupArn: initialRule.Actions?.[0]?.TargetGroupArn ?? "",
    })
    // form is intentionally excluded so the reset only fires on identity change of the source.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [initialRule, open, mode])

  const handleConfirm = form.handleSubmit(onSubmit)

  return (
    <AlertDialog onOpenChange={onOpenChange} open={open}>
      <AlertDialogContent
        className="grid-cols-[minmax(0,1fr)]"
        style={{ maxWidth: "40rem", width: "calc(100vw - 2rem)" }}
      >
        <AlertDialogHeader>
          <AlertDialogTitle>
            {mode === "add" ? "Add rule" : "Edit rule"}
          </AlertDialogTitle>
          <AlertDialogDescription>
            Routing rules match incoming requests and forward them to a target
            group. Rules evaluate in priority order; the listener default
            applies when no rule matches.
          </AlertDialogDescription>
        </AlertDialogHeader>

        <form
          className="min-w-0 space-y-4"
          onSubmit={(e) => {
            e.preventDefault()
            void handleConfirm()
          }}
        >
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
                onClick={() => append({ field: "path-pattern", rawValues: "" })}
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
                onRemove={fields.length > 1 ? () => remove(idx) : undefined}
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
        </form>

        <AlertDialogFooter>
          <AlertDialogCancel disabled={isPending}>Cancel</AlertDialogCancel>
          <AlertDialogAction disabled={isPending} onClick={handleConfirm}>
            {actionLabel(mode, isPending)}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
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
                  <SelectItem value="path-pattern">Path pattern</SelectItem>
                  <SelectItem value="host-header">Host header</SelectItem>
                  <SelectItem value="http-header">HTTP header</SelectItem>
                  <SelectItem value="http-request-method">
                    HTTP method
                  </SelectItem>
                  <SelectItem value="source-ip">Source IP CIDR</SelectItem>
                  <SelectItem value="query-string">Query string</SelectItem>
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
          placeholder={placeholderForField(fieldName)}
          {...form.register(`conditions.${index}.rawValues`)}
        />
        <FieldError errors={[errors?.rawValues]} />
      </Field>
    </div>
  )
}

function placeholderForField(field: string): string {
  switch (field) {
    case "path-pattern": {
      return "/api/*"
    }
    case "host-header": {
      return "*.example.com"
    }
    case "http-header": {
      return "acme"
    }
    case "http-request-method": {
      return "GET, POST"
    }
    case "source-ip": {
      return "10.0.0.0/8"
    }
    case "query-string": {
      return "key=value"
    }
    default: {
      return ""
    }
  }
}

const SUPPORTED_FIELDS = new Set<string>([
  "host-header",
  "path-pattern",
  "http-header",
  "http-request-method",
  "source-ip",
  "query-string",
])

function isSupportedField(
  value: string | undefined,
): value is RuleConditionFormData["field"] {
  return value !== undefined && SUPPORTED_FIELDS.has(value)
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

function actionLabel(mode: "add" | "edit", isPending: boolean): string {
  if (isPending) {
    return mode === "add" ? "Adding…" : "Saving…"
  }
  return mode === "add" ? "Add rule" : "Save changes"
}
