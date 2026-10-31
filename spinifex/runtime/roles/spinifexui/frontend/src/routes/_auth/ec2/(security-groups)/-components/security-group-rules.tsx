import type { IpPermission, SecurityGroup } from "@aws-sdk/client-ec2"
import { Link } from "@tanstack/react-router"
import { Plus, X } from "lucide-react"
import { useState } from "react"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { PanelSection } from "@/components/detail-panel"
import { ErrorBanner } from "@/components/error-banner"
import { Button } from "@/components/ui/button"
import {
  useRevokeSecurityGroupEgress,
  useRevokeSecurityGroupIngress,
} from "@/mutations/ec2"

import { describeRulePorts } from "../../(instances)/-components/instance-overview"
import { AddSecurityGroupRuleForm } from "./add-security-group-rule-form"

type Direction = "ingress" | "egress"

// A rule's source/destination: a CIDR, a referenced security group (the AWS
// default SG references itself), an IPv6 CIDR or a prefix list. The console
// revokes only the first two.
interface RuleSource {
  kind: "cidr" | "group" | "ipv6" | "prefix"
  value: string
}

function ruleSources(rule: IpPermission): RuleSource[] {
  return [
    ...(rule.IpRanges ?? []).map((range): RuleSource => ({
      kind: "cidr",
      value: range.CidrIp ?? "",
    })),
    ...(rule.UserIdGroupPairs ?? []).map((pair): RuleSource => ({
      kind: "group",
      value: pair.GroupId ?? "",
    })),
    ...(rule.Ipv6Ranges ?? []).map((range): RuleSource => ({
      kind: "ipv6",
      value: range.CidrIpv6 ?? "",
    })),
    ...(rule.PrefixListIds ?? []).map((list): RuleSource => ({
      kind: "prefix",
      value: list.PrefixListId ?? "",
    })),
  ]
}

interface RuleRowProps {
  rule: IpPermission
  source: RuleSource
  groupId: string
  direction: Direction
}

function RuleRow({ rule, source, groupId, direction }: RuleRowProps) {
  const revokeIngress = useRevokeSecurityGroupIngress()
  const revokeEgress = useRevokeSecurityGroupEgress()
  const mutation = direction === "ingress" ? revokeIngress : revokeEgress
  const [confirm, setConfirm] = useState(false)
  const ports = describeRulePorts(rule)
  const self = source.kind === "group" && source.value === groupId
  const preposition = direction === "ingress" ? "from" : "to"
  const revocable = source.kind === "cidr" || source.kind === "group"

  async function onRemove() {
    try {
      await mutation.mutateAsync({
        groupId,
        ipProtocol: rule.IpProtocol ?? "-1",
        fromPort: rule.FromPort ?? -1,
        toPort: rule.ToPort ?? -1,
        ...(source.kind === "group"
          ? { sourceGroupId: source.value }
          : { cidrIp: source.value }),
      })
    } catch {
      // Shown by the banner below.
    } finally {
      setConfirm(false)
    }
  }

  return (
    <li className="flex flex-col gap-1 py-1.5">
      <div className="flex items-center justify-between gap-3">
        <div className="flex min-w-0 items-baseline gap-2">
          <span className="shrink-0 font-mono text-xs">{ports}</span>
          <span className="text-xs text-muted-foreground">{preposition}</span>
          {source.kind === "group" ? (
            <Link
              className="truncate font-mono text-xs text-primary hover:underline"
              params={{ id: source.value }}
              title={source.value}
              to="/ec2/describe-security-groups/$id"
            >
              {self ? "this security group" : source.value}
            </Link>
          ) : (
            <span className="truncate font-mono text-xs">{source.value}</span>
          )}
        </div>
        <Button
          aria-label="Remove rule"
          disabled={!revocable || mutation.isPending}
          onClick={() => {
            setConfirm(true)
          }}
          size="icon-xs"
          variant="ghost"
        >
          <X />
        </Button>
      </div>
      {mutation.error && (
        <ErrorBanner error={mutation.error} msg="Failed to remove rule" />
      )}
      <DeleteConfirmationDialog
        confirmLabel="Remove"
        description={`Remove the ${direction === "ingress" ? "inbound" : "outbound"} rule ${ports} ${preposition} ${self ? `${source.value} (this security group)` : source.value} from ${groupId}?`}
        isPending={mutation.isPending}
        onConfirm={() => {
          void onRemove()
        }}
        onOpenChange={setConfirm}
        open={confirm}
        pendingLabel="Removing…"
        title="Remove rule"
      />
    </li>
  )
}

interface SecurityGroupRulesProps {
  sg: SecurityGroup
  direction: Direction
}

// One direction's rules, one row per source as AWS revokes them, with add and
// remove as the group's own page offers them.
export function SecurityGroupRules({ sg, direction }: SecurityGroupRulesProps) {
  const groupId = sg.GroupId ?? ""
  const [adding, setAdding] = useState(false)

  const rules =
    (direction === "ingress" ? sg.IpPermissions : sg.IpPermissionsEgress) ?? []

  const rows = rules.flatMap((rule) =>
    ruleSources(rule).map((source) => ({ rule, source })),
  )

  const label = direction === "ingress" ? "Inbound" : "Outbound"

  return (
    <PanelSection title={`${label} rules (${rows.length})`}>
      <div className="flex flex-col gap-2">
        {rows.length > 0 ? (
          <ul className="divide-y text-sm">
            {rows.map(({ rule, source }) => (
              <RuleRow
                direction={direction}
                groupId={groupId}
                key={`${rule.IpProtocol}-${rule.FromPort}-${rule.ToPort}-${source.kind}-${source.value}`}
                rule={rule}
                source={source}
              />
            ))}
          </ul>
        ) : (
          <p className="text-sm text-muted-foreground">
            No {label.toLowerCase()} rules.
          </p>
        )}
        <div>
          <Button
            aria-label={`Add ${label.toLowerCase()} rule`}
            onClick={() => {
              setAdding(true)
            }}
            size="sm"
            variant="outline"
          >
            <Plus data-icon="inline-start" />
            Add rule
          </Button>
        </div>
      </div>
      {adding && (
        <AddSecurityGroupRuleForm
          direction={direction}
          groupId={groupId}
          onClose={() => {
            setAdding(false)
          }}
        />
      )}
    </PanelSection>
  )
}
