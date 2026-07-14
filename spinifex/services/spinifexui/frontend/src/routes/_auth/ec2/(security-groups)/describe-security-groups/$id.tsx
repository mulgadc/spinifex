import type { IpPermission } from "@aws-sdk/client-ec2"
import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery } from "@tanstack/react-query"
import { createFileRoute, Link, useNavigate } from "@tanstack/react-router"
import { Plus, Trash2, X } from "lucide-react"
import { useState } from "react"
import { Controller, useForm, useWatch } from "react-hook-form"

import { BackLink } from "@/components/back-link"
import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
import { ErrorBanner } from "@/components/error-banner"
import { PageHeading } from "@/components/page-heading"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
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
  useAuthorizeSecurityGroupEgress,
  useAuthorizeSecurityGroupIngress,
  useDeleteSecurityGroup,
  useRevokeSecurityGroupEgress,
  useRevokeSecurityGroupIngress,
} from "@/mutations/ec2"
import { ec2SecurityGroupQueryOptions } from "@/queries/ec2"
import {
  type SecurityGroupRuleFormData,
  securityGroupRuleSchema,
} from "@/types/ec2"

export const Route = createFileRoute(
  "/_auth/ec2/(security-groups)/describe-security-groups/$id",
)({
  loader: async ({ context, params }) => {
    await context.queryClient.ensureQueryData(
      ec2SecurityGroupQueryOptions(params.id),
    )
  },
  head: ({ params }) => ({
    meta: [
      {
        title: `${params.id} | Security Group | Mulga`,
      },
    ],
  }),
  component: SecurityGroupDetail,
})

function formatProtocol(protocol: string | undefined): string {
  if (!protocol || protocol === "-1") {
    return "All traffic"
  }
  const upper = protocol.toUpperCase()
  if (upper === "TCP" || upper === "UDP" || upper === "ICMP") {
    return upper
  }
  return protocol
}

function formatPortRange(
  fromPort: number | undefined,
  toPort: number | undefined,
  protocol: string | undefined,
): string {
  if (!protocol || protocol === "-1") {
    return "All"
  }
  if (fromPort === undefined || toPort === undefined) {
    return "All"
  }
  if (fromPort === -1 && toPort === -1) {
    return "All"
  }
  if (fromPort === toPort) {
    return fromPort.toString()
  }
  return `${fromPort}-${toPort}`
}

interface AddRuleDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  direction: "ingress" | "egress"
  groupId: string
}

function AddRuleDialog({
  open,
  onOpenChange,
  direction,
  groupId,
}: AddRuleDialogProps) {
  const authorizeIngress = useAuthorizeSecurityGroupIngress()
  const authorizeEgress = useAuthorizeSecurityGroupEgress()
  const mutation = direction === "ingress" ? authorizeIngress : authorizeEgress

  const {
    control,
    handleSubmit,
    register,
    reset,
    formState: { errors },
  } = useForm<SecurityGroupRuleFormData>({
    resolver: zodResolver(securityGroupRuleSchema),
    defaultValues: {
      ipProtocol: "tcp",
      fromPort: 0,
      toPort: 0,
      cidrIp: "0.0.0.0/0",
    },
  })

  const selectedProtocol = useWatch({ control, name: "ipProtocol" })
  const portsDisabled = selectedProtocol === "-1" || selectedProtocol === "icmp"

  const onSubmit = async (data: SecurityGroupRuleFormData) => {
    const params = {
      groupId,
      ipProtocol: data.ipProtocol,
      fromPort: portsDisabled ? -1 : data.fromPort,
      toPort: portsDisabled ? -1 : data.toPort,
      cidrIp: data.cidrIp,
    }
    await mutation.mutateAsync(params)
    reset()
    onOpenChange(false)
  }

  return (
    <AlertDialog onOpenChange={onOpenChange} open={open}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>
            Add {direction === "ingress" ? "Inbound" : "Outbound"} Rule
          </AlertDialogTitle>
        </AlertDialogHeader>

        {mutation.error && (
          <ErrorBanner error={mutation.error} msg="Failed to add rule" />
        )}

        <form
          className="space-y-4"
          id="add-rule-form"
          onSubmit={handleSubmit(onSubmit)}
        >
          <Field>
            <FieldTitle>
              <label htmlFor="ipProtocol">Protocol</label>
            </FieldTitle>
            <Controller
              control={control}
              name="ipProtocol"
              render={({ field }) => (
                <Select
                  onValueChange={(value) => field.onChange(value)}
                  value={field.value}
                >
                  <SelectTrigger className="w-full" id="ipProtocol">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="tcp">TCP</SelectItem>
                    <SelectItem value="udp">UDP</SelectItem>
                    <SelectItem value="icmp">ICMP</SelectItem>
                    <SelectItem value="-1">All traffic (-1)</SelectItem>
                  </SelectContent>
                </Select>
              )}
            />
            <FieldError errors={[errors.ipProtocol]} />
          </Field>

          <div className="grid grid-cols-2 gap-4">
            <Field>
              <FieldTitle>
                <label htmlFor="fromPort">From Port</label>
              </FieldTitle>
              <Input
                aria-invalid={!!errors.fromPort}
                disabled={portsDisabled}
                id="fromPort"
                type="number"
                {...register("fromPort", { valueAsNumber: true })}
              />
              <FieldError errors={[errors.fromPort]} />
            </Field>

            <Field>
              <FieldTitle>
                <label htmlFor="toPort">To Port</label>
              </FieldTitle>
              <Input
                aria-invalid={!!errors.toPort}
                disabled={portsDisabled}
                id="toPort"
                type="number"
                {...register("toPort", { valueAsNumber: true })}
              />
              <FieldError errors={[errors.toPort]} />
            </Field>
          </div>

          <Field>
            <FieldTitle>
              <label htmlFor="cidrIp">
                {direction === "ingress" ? "Source" : "Destination"} CIDR
              </label>
            </FieldTitle>
            <Input
              aria-invalid={!!errors.cidrIp}
              id="cidrIp"
              placeholder="0.0.0.0/0"
              {...register("cidrIp")}
            />
            <FieldError errors={[errors.cidrIp]} />
          </Field>
        </form>

        <AlertDialogFooter>
          <AlertDialogCancel
            onClick={() => {
              reset()
              mutation.reset()
            }}
          >
            Cancel
          </AlertDialogCancel>
          <AlertDialogAction
            disabled={mutation.isPending}
            form="add-rule-form"
            type="submit"
          >
            {mutation.isPending ? "Adding\u2026" : "Add Rule"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}

// A rule's source/destination is either a CIDR or a referenced security group
// (UserIdGroupPair). The AWS default SG uses a self-referencing group source.
interface RuleSource {
  kind: "cidr" | "group"
  value: string
}

function ruleSources(rule: IpPermission): RuleSource[] {
  return [
    ...(rule.IpRanges ?? []).map(
      (range): RuleSource => ({ kind: "cidr", value: range.CidrIp ?? "" }),
    ),
    ...(rule.UserIdGroupPairs ?? []).map(
      (pair): RuleSource => ({ kind: "group", value: pair.GroupId ?? "" }),
    ),
  ]
}

interface RuleRowProps {
  rule: IpPermission
  source: RuleSource
  groupId: string
  direction: "ingress" | "egress"
}

function RuleRow({ rule, source, groupId, direction }: RuleRowProps) {
  const revokeIngress = useRevokeSecurityGroupIngress()
  const revokeEgress = useRevokeSecurityGroupEgress()
  const mutation = direction === "ingress" ? revokeIngress : revokeEgress

  const handleRemove = async () => {
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
      // Error is stored in mutation.error and rendered below
    }
  }

  const sourceLabel =
    source.kind === "group" && source.value === groupId
      ? `${source.value} (this security group)`
      : source.value

  return (
    <div className="space-y-0">
      <div className="flex items-center justify-between border-b px-4 py-3 last:border-b-0">
        <div className="grid flex-1 grid-cols-3 gap-4 text-sm">
          <div>
            <span className="text-muted-foreground">Protocol: </span>
            {formatProtocol(rule.IpProtocol)}
          </div>
          <div>
            <span className="text-muted-foreground">Port Range: </span>
            {formatPortRange(rule.FromPort, rule.ToPort, rule.IpProtocol)}
          </div>
          <div>
            <span className="text-muted-foreground">
              {direction === "ingress" ? "Source" : "Destination"}:{" "}
            </span>
            {sourceLabel}
          </div>
        </div>
        <Button
          disabled={mutation.isPending}
          onClick={handleRemove}
          size="icon-xs"
          variant="ghost"
        >
          <X className="size-3" />
        </Button>
      </div>
      {mutation.error && (
        <div className="px-4 pb-2">
          <ErrorBanner error={mutation.error} msg="Failed to remove rule" />
        </div>
      )}
    </div>
  )
}

function SecurityGroupDetail() {
  const { id } = Route.useParams()
  const navigate = useNavigate()
  const { data } = useSuspenseQuery(ec2SecurityGroupQueryOptions(id))
  const sg = data.SecurityGroups?.[0]
  const deleteMutation = useDeleteSecurityGroup()
  const [showDeleteDialog, setShowDeleteDialog] = useState(false)
  const [showIngressDialog, setShowIngressDialog] = useState(false)
  const [showEgressDialog, setShowEgressDialog] = useState(false)

  const handleDelete = async () => {
    try {
      await deleteMutation.mutateAsync(id)
      navigate({ to: "/ec2/describe-security-groups" })
    } finally {
      setShowDeleteDialog(false)
    }
  }

  if (!sg?.GroupId) {
    return (
      <>
        <BackLink to="/ec2/describe-security-groups">
          Back to Security Groups
        </BackLink>
        <p className="text-muted-foreground">Security group not found.</p>
      </>
    )
  }

  const ingressRules = sg.IpPermissions ?? []
  const egressRules = sg.IpPermissionsEgress ?? []

  return (
    <>
      <BackLink to="/ec2/describe-security-groups">
        Back to Security Groups
      </BackLink>

      {deleteMutation.error && (
        <ErrorBanner
          error={deleteMutation.error}
          msg="Failed to delete security group"
        />
      )}

      <div className="space-y-6">
        <PageHeading
          actions={
            <div className="flex items-center gap-2">
              <Button
                onClick={() => setShowDeleteDialog(true)}
                size="sm"
                variant="destructive"
              >
                <Trash2 className="size-4" />
                Delete
              </Button>
            </div>
          }
          subtitle="Security Group Details"
          title={sg.GroupName ? `${sg.GroupId} (${sg.GroupName})` : sg.GroupId}
        />

        <DetailCard>
          <DetailCard.Header>Security Group Information</DetailCard.Header>
          <DetailCard.Content>
            <DetailRow label="Group ID" value={sg.GroupId} />
            <DetailRow label="Group Name" value={sg.GroupName} />
            <DetailRow label="Description" value={sg.Description} />
            <DetailRow
              label="VPC ID"
              value={
                sg.VpcId ? (
                  <Link
                    className="text-primary hover:underline"
                    params={{ id: sg.VpcId }}
                    to="/ec2/describe-vpcs/$id"
                  >
                    {sg.VpcId}
                  </Link>
                ) : (
                  "\u2014"
                )
              }
            />
            <DetailRow label="Owner ID" value={sg.OwnerId} />
          </DetailCard.Content>
        </DetailCard>

        <DetailCard>
          <div className="flex items-center justify-between border-b p-4">
            <h2 className="font-semibold">
              Inbound Rules ({ingressRules.length})
            </h2>
            <Button
              onClick={() => setShowIngressDialog(true)}
              size="sm"
              variant="outline"
            >
              <Plus className="size-4" />
              Add Rule
            </Button>
          </div>
          {ingressRules.length > 0 ? (
            <div>
              {ingressRules.flatMap((rule) =>
                ruleSources(rule).map((source) => (
                  <RuleRow
                    direction="ingress"
                    groupId={sg.GroupId ?? ""}
                    key={`${rule.IpProtocol}-${rule.FromPort}-${rule.ToPort}-${source.kind}-${source.value}`}
                    rule={rule}
                    source={source}
                  />
                )),
              )}
            </div>
          ) : (
            <p className="p-4 text-sm text-muted-foreground">
              No inbound rules.
            </p>
          )}
        </DetailCard>

        <DetailCard>
          <div className="flex items-center justify-between border-b p-4">
            <h2 className="font-semibold">
              Outbound Rules ({egressRules.length})
            </h2>
            <Button
              onClick={() => setShowEgressDialog(true)}
              size="sm"
              variant="outline"
            >
              <Plus className="size-4" />
              Add Rule
            </Button>
          </div>
          {egressRules.length > 0 ? (
            <div>
              {egressRules.flatMap((rule) =>
                ruleSources(rule).map((source) => (
                  <RuleRow
                    direction="egress"
                    groupId={sg.GroupId ?? ""}
                    key={`${rule.IpProtocol}-${rule.FromPort}-${rule.ToPort}-${source.kind}-${source.value}`}
                    rule={rule}
                    source={source}
                  />
                )),
              )}
            </div>
          ) : (
            <p className="p-4 text-sm text-muted-foreground">
              No outbound rules.
            </p>
          )}
        </DetailCard>
      </div>

      <DeleteConfirmationDialog
        description={`Are you sure you want to delete the security group "${sg.GroupId}"? This action cannot be undone.`}
        isPending={deleteMutation.isPending}
        onConfirm={handleDelete}
        onOpenChange={setShowDeleteDialog}
        open={showDeleteDialog}
        title="Delete Security Group"
      />

      <AddRuleDialog
        direction="ingress"
        groupId={sg.GroupId}
        onOpenChange={setShowIngressDialog}
        open={showIngressDialog}
      />

      <AddRuleDialog
        direction="egress"
        groupId={sg.GroupId}
        onOpenChange={setShowEgressDialog}
        open={showEgressDialog}
      />
    </>
  )
}
