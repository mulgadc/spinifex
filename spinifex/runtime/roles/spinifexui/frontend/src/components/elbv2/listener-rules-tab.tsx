import type {
  Rule,
  RuleCondition,
  TargetGroup,
} from "@aws-sdk/client-elastic-load-balancing-v2"
import { useSuspenseQuery } from "@tanstack/react-query"
import { Pencil, Trash2 } from "lucide-react"
import { useState } from "react"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { PanelField, PanelFields } from "@/components/detail-panel"
import {
  ListenerRuleForm,
  collectConditionValues,
  toRuleCondInput,
} from "@/components/elbv2/listener-rule-form"
import { ErrorBanner } from "@/components/error-banner"
import { Button } from "@/components/ui/button"
import { useCreateRule, useDeleteRule, useModifyRule } from "@/mutations/elbv2"
import { elbv2ListenerRulesQueryOptions } from "@/queries/elbv2"
import type { CreateRuleFormData } from "@/types/elbv2"

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
      {deleteMutation.error && (
        <ErrorBanner error={deleteMutation.error} msg="Failed to delete rule" />
      )}

      <div className="flex items-center justify-between">
        <p className="text-xs text-muted-foreground">
          {rules.length} rule{rules.length === 1 ? "" : "s"} (including default)
        </p>
        <Button
          onClick={() => {
            setAddOpen(true)
          }}
          size="sm"
        >
          Add rule
        </Button>
      </div>

      <ul className="flex flex-col gap-2">
        {rules.map((rule) => (
          <li
            className="rounded-md border bg-card"
            key={rule.RuleArn ?? rule.Priority ?? "default"}
          >
            <div className="flex items-center justify-between gap-2 py-1 pr-1 pl-3">
              <span className="text-sm font-medium">
                Priority {rule.Priority}
              </span>
              {rule.IsDefault === true ? (
                <span className="pr-2 text-xs text-muted-foreground">
                  Edit via listener
                </span>
              ) : (
                <div className="flex gap-1">
                  <Button
                    aria-label={`Edit rule ${rule.Priority}`}
                    onClick={() => {
                      setEditTarget(rule)
                    }}
                    size="icon-sm"
                    variant="ghost"
                  >
                    <Pencil />
                  </Button>
                  <Button
                    aria-label={`Delete rule ${rule.Priority}`}
                    onClick={() => {
                      setDeleteTarget(rule)
                    }}
                    size="icon-sm"
                    variant="ghost"
                  >
                    <Trash2 />
                  </Button>
                </div>
              )}
            </div>
            <div className="border-t px-3">
              {/* Conditions can be long, so they wrap rather than truncate. */}
              <p className="border-b py-1.5 text-xs break-words">
                {formatConditions(rule.Conditions, rule.IsDefault === true)}
              </p>
              <PanelFields>
                <PanelField label="Forward to">
                  {formatActions(rule, tgNameByArn)}
                </PanelField>
              </PanelFields>
            </div>
          </li>
        ))}
      </ul>

      {/* Mounted only while open so every session starts from fresh values. */}
      {addOpen && (
        <ListenerRuleForm
          error={createMutation.error}
          isPending={createMutation.isPending}
          listenerArn={listenerArn}
          onClose={() => {
            createMutation.reset()
            setAddOpen(false)
          }}
          onSubmit={handleCreate}
          targetGroups={targetGroups}
        />
      )}

      {editTarget && (
        <ListenerRuleForm
          error={modifyMutation.error}
          isPending={modifyMutation.isPending}
          key={editTarget.RuleArn}
          listenerArn={listenerArn}
          onClose={() => {
            modifyMutation.reset()
            setEditTarget(undefined)
          }}
          onSubmit={handleEdit}
          rule={editTarget}
          targetGroups={targetGroups}
        />
      )}

      <DeleteConfirmationDialog
        description={
          deleteTarget
            ? `Delete rule with priority ${deleteTarget.Priority}? This cannot be undone.`
            : ""
        }
        isPending={deleteMutation.isPending}
        onConfirm={handleDelete}
        onOpenChange={(open) => {
          if (!open) {
            setDeleteTarget(undefined)
          }
        }}
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
