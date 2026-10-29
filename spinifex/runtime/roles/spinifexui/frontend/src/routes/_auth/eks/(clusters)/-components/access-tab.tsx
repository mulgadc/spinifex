import { zodResolver } from "@hookform/resolvers/zod"
import { useQuery, useSuspenseQuery } from "@tanstack/react-query"
import { Plus, Trash2 } from "lucide-react"
import { useState } from "react"
import { useForm } from "react-hook-form"

import { DeleteConfirmationDialog } from "@/components/delete-confirmation-dialog"
import {
  PanelField,
  PanelFields,
  PanelSection,
} from "@/components/detail-panel"
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
import { arnResourceId } from "@/lib/utils"
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
    <div className="rounded-md border px-3 py-2">
      <div className="flex items-center justify-between gap-2">
        <span className="min-w-0 truncate font-mono text-xs">
          {principalArn}
        </span>
        <Button
          aria-label="Delete access entry"
          onClick={() => {
            onDelete(principalArn)
          }}
          size="icon-sm"
          variant="ghost"
        >
          <Trash2 />
        </Button>
      </div>
      <PanelFields>
        <PanelField label="Type">{entry?.type}</PanelField>
        <PanelField label="Kubernetes groups">
          {entry?.kubernetesGroups?.length
            ? entry.kubernetesGroups.join(", ")
            : undefined}
        </PanelField>
        {associated.length > 0 ? (
          associated.map((p, index) => (
            <div
              className="flex items-center justify-between gap-4 py-1.5 text-sm"
              key={p.policyArn}
            >
              <span className="shrink-0 text-muted-foreground">
                {index === 0 ? "Access policies" : ""}
              </span>
              <span className="flex min-w-0 items-center gap-1">
                <span className="truncate">{arnResourceId(p.policyArn)}</span>
                <Button
                  onClick={() => {
                    disassociate.mutate({
                      clusterName,
                      principalArn,
                      policyArn: p.policyArn ?? "",
                    })
                  }}
                  size="xs"
                  variant="ghost"
                >
                  Remove
                </Button>
              </span>
            </div>
          ))
        ) : (
          <PanelField label="Access policies" />
        )}
      </PanelFields>
      <div className="flex items-center gap-2 pt-2">
        <select
          aria-label="Access policy"
          className="h-7 min-w-0 flex-1 rounded-md border border-input bg-input/20 px-2 text-sm"
          onChange={(e) => {
            setPolicyArn(e.target.value)
          }}
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
          onClick={() => {
            associate.mutate({
              clusterName,
              principalArn,
              policyArn,
              accessScopeType: "cluster",
            })
          }}
          size="sm"
        >
          Associate
        </Button>
      </div>
    </div>
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
      <PanelSection title={`Access entries (${entries.length})`}>
        <div className="flex flex-col gap-3 py-1">
          <div>
            <Button
              onClick={() => {
                setShowAdd(true)
              }}
              size="sm"
              variant="outline"
            >
              <Plus data-icon="inline-start" />
              Add access entry
            </Button>
          </div>
          {entries.length > 0 ? (
            entries.map((arn) => (
              <AccessEntryRow
                clusterName={clusterName}
                key={arn}
                onDelete={setPendingDelete}
                principalArn={arn}
              />
            ))
          ) : (
            <p className="text-sm text-muted-foreground">No access entries.</p>
          )}
        </div>
      </PanelSection>

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
            {
              onSuccess: () => {
                setPendingDelete(undefined)
              },
            },
          )
        }}
        onOpenChange={(o) => {
          if (!o) {
            setPendingDelete(undefined)
          }
        }}
        open={pendingDelete !== undefined}
        title="Delete access entry"
      />
    </>
  )
}
