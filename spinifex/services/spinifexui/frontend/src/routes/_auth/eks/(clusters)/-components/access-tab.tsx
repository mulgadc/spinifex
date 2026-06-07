import { zodResolver } from "@hookform/resolvers/zod"
import { useQuery, useSuspenseQuery } from "@tanstack/react-query"
import { Trash2 } from "lucide-react"
import { useState } from "react"
import { useForm } from "react-hook-form"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import { DetailCard } from "@/components/detail-card"
import { DetailRow } from "@/components/detail-row"
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
  useAssociateAccessPolicy,
  useCreateAccessEntry,
  useDeleteAccessEntry,
  useDisassociateAccessPolicy,
} from "@/mutations/eks"
import {
  eksAccessEntriesQueryOptions,
  eksAccessEntryQueryOptions,
  eksAssociatedAccessPoliciesQueryOptions,
} from "@/queries/eks"
import {
  type CreateAccessEntryFormValues,
  createAccessEntrySchema,
  EKS_ACCESS_POLICIES,
} from "@/types/eks"

function shortPolicyName(arn: string | undefined): string {
  return arn?.split("/").pop() ?? arn ?? ""
}

function AccessEntryRow({
  clusterName,
  principalArn,
  onDelete,
}: {
  clusterName: string
  principalArn: string
  onDelete: (arn: string) => void
}) {
  const { data: entryData } = useQuery(
    eksAccessEntryQueryOptions(clusterName, principalArn),
  )
  const { data: policyData } = useQuery(
    eksAssociatedAccessPoliciesQueryOptions(clusterName, principalArn),
  )
  const associate = useAssociateAccessPolicy()
  const disassociate = useDisassociateAccessPolicy()
  const [policyArn, setPolicyArn] = useState<string>(EKS_ACCESS_POLICIES[0].arn)

  const entry = entryData?.accessEntry
  const associated = policyData?.associatedAccessPolicies ?? []

  return (
    <DetailCard>
      <DetailCard.Header>
        <div className="flex items-center justify-between gap-2">
          <span className="truncate font-mono text-xs">{principalArn}</span>
          <Button
            onClick={() => onDelete(principalArn)}
            size="icon"
            variant="ghost"
          >
            <Trash2 className="size-4" />
          </Button>
        </div>
      </DetailCard.Header>
      <DetailCard.Content>
        <DetailRow label="Type" value={entry?.type} />
        <DetailRow
          label="Kubernetes groups"
          value={
            entry?.kubernetesGroups?.length
              ? entry.kubernetesGroups.join(", ")
              : "—"
          }
        />
        <DetailRow
          label="Access policies"
          value={
            associated.length > 0 ? (
              <div className="space-y-1">
                {associated.map((p) => (
                  <div
                    className="flex items-center justify-between gap-2"
                    key={p.policyArn}
                  >
                    <span>{shortPolicyName(p.policyArn)}</span>
                    <Button
                      onClick={() =>
                        disassociate.mutate({
                          clusterName,
                          principalArn,
                          policyArn: p.policyArn ?? "",
                        })
                      }
                      size="sm"
                      variant="ghost"
                    >
                      Remove
                    </Button>
                  </div>
                ))}
              </div>
            ) : (
              "—"
            )
          }
        />
        <div className="flex items-center gap-2 pt-2">
          <select
            aria-label="Access policy"
            className="h-7 flex-1 rounded-md border border-input bg-input/20 px-2 text-sm"
            onChange={(e) => setPolicyArn(e.target.value)}
            value={policyArn}
          >
            {EKS_ACCESS_POLICIES.map((p) => (
              <option key={p.arn} value={p.arn}>
                {p.name}
              </option>
            ))}
          </select>
          <Button
            disabled={associate.isPending}
            onClick={() =>
              associate.mutate({
                clusterName,
                principalArn,
                policyArn,
                accessScopeType: "cluster",
              })
            }
            size="sm"
          >
            Associate
          </Button>
        </div>
      </DetailCard.Content>
    </DetailCard>
  )
}

function AddAccessEntryDialog({
  clusterName,
  open,
  onOpenChange,
}: {
  clusterName: string
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  const createEntry = useCreateAccessEntry()
  const {
    formState: { errors },
    handleSubmit,
    register,
    reset,
  } = useForm<CreateAccessEntryFormValues>({
    resolver: zodResolver(createAccessEntrySchema),
    defaultValues: { principalArn: "", kubernetesGroups: "" },
  })

  const onSubmit = async (data: CreateAccessEntryFormValues) => {
    await createEntry.mutateAsync({
      clusterName,
      principalArn: data.principalArn,
      kubernetesGroups: data.kubernetesGroups
        .split(",")
        .map((g) => g.trim())
        .filter(Boolean),
    })
    reset()
    onOpenChange(false)
  }

  return (
    <AlertDialog onOpenChange={onOpenChange} open={open}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Add access entry</AlertDialogTitle>
        </AlertDialogHeader>

        <form className="space-y-4" id="add-access-entry-form">
          <Field>
            <FieldTitle>
              <label htmlFor="ae-principal">IAM principal ARN</label>
            </FieldTitle>
            <Input
              aria-invalid={!!errors.principalArn}
              id="ae-principal"
              placeholder="arn:aws:iam::000000000000:role/my-role"
              {...register("principalArn")}
            />
            <FieldError errors={[errors.principalArn]} />
          </Field>

          <Field>
            <FieldTitle>
              <label htmlFor="ae-groups">Kubernetes groups</label>
            </FieldTitle>
            <Input
              id="ae-groups"
              placeholder="system:masters"
              {...register("kubernetesGroups")}
            />
          </Field>
        </form>

        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            disabled={createEntry.isPending}
            onClick={(e) => {
              e.preventDefault()
              void handleSubmit(onSubmit)()
            }}
          >
            {createEntry.isPending ? "Creating…" : "Create"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}

export function AccessTab({ clusterName }: { clusterName: string }) {
  const { data } = useSuspenseQuery(eksAccessEntriesQueryOptions(clusterName))
  const deleteEntry = useDeleteAccessEntry()
  const [showAdd, setShowAdd] = useState(false)
  const [pendingDelete, setPendingDelete] = useState<string | undefined>()

  const entries = data.accessEntries ?? []

  return (
    <>
      <div className="mb-4 flex justify-end">
        <Button onClick={() => setShowAdd(true)} size="sm">
          Add access entry
        </Button>
      </div>

      {entries.length > 0 ? (
        <div className="space-y-4">
          {entries.map((arn) => (
            <AccessEntryRow
              clusterName={clusterName}
              key={arn}
              onDelete={setPendingDelete}
              principalArn={arn}
            />
          ))}
        </div>
      ) : (
        <p className="text-muted-foreground">No access entries.</p>
      )}

      <AddAccessEntryDialog
        clusterName={clusterName}
        onOpenChange={setShowAdd}
        open={showAdd}
      />

      <DeleteConfirmationDialog
        description={`Delete access entry for "${pendingDelete ?? ""}"?`}
        isPending={deleteEntry.isPending}
        onConfirm={() => {
          if (!pendingDelete) {
            return
          }
          deleteEntry.mutate(
            { clusterName, principalArn: pendingDelete },
            { onSuccess: () => setPendingDelete(undefined) },
          )
        }}
        onOpenChange={(o) => !o && setPendingDelete(undefined)}
        open={pendingDelete !== undefined}
        title="Delete access entry"
      />
    </>
  )
}
