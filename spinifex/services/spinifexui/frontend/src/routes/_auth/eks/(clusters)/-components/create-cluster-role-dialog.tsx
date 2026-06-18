import { useEffect, useState } from "react"

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
import { Field, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { useAttachRolePolicy, useCreateRole } from "@/mutations/iam"
import { PolicyDocumentViewer } from "@/routes/_auth/iam/-components/policy-document-viewer"
import {
  AMAZON_EKS_CLUSTER_POLICY_ARN,
  EKS_CLUSTER_ASSUME_ROLE_POLICY_DOCUMENT,
} from "@/types/eks"

interface CreateClusterRoleDialogProps {
  clusterName: string
  open: boolean
  onOpenChange: (open: boolean) => void
  onCreated: (roleArn: string) => void
}

function defaultRoleName(clusterName: string): string {
  const base = clusterName.trim() || "cluster"
  return `${base}-eks-cluster-role`
}

export function CreateClusterRoleDialog({
  clusterName,
  open,
  onOpenChange,
  onCreated,
}: CreateClusterRoleDialogProps) {
  const createRole = useCreateRole()
  const attachPolicy = useAttachRolePolicy()
  const [roleName, setRoleName] = useState(defaultRoleName(clusterName))
  const [formError, setFormError] = useState<string | null>(null)

  useEffect(() => {
    if (open) {
      setRoleName(defaultRoleName(clusterName))
      setFormError(null)
    }
  }, [open, clusterName])

  const isPending = createRole.isPending || attachPolicy.isPending

  function handleClose(nextOpen: boolean) {
    if (!nextOpen) {
      setFormError(null)
    }
    onOpenChange(nextOpen)
  }

  async function handleCreate() {
    setFormError(null)
    const name = roleName.trim()
    try {
      const result = await createRole.mutateAsync({
        roleName: name,
        assumeRolePolicyDocument: EKS_CLUSTER_ASSUME_ROLE_POLICY_DOCUMENT,
      })
      await attachPolicy.mutateAsync({
        roleName: name,
        policyArn: AMAZON_EKS_CLUSTER_POLICY_ARN,
      })
      const arn = result.Role?.Arn
      if (!arn) {
        setFormError("Role was created but no ARN was returned.")
        return
      }
      onCreated(arn)
      onOpenChange(false)
    } catch (error) {
      setFormError(
        error instanceof Error ? error.message : "Failed to create role.",
      )
    }
  }

  return (
    <AlertDialog onOpenChange={handleClose} open={open}>
      <AlertDialogContent className="max-w-lg sm:max-w-lg">
        <AlertDialogHeader>
          <AlertDialogTitle>Create cluster IAM role</AlertDialogTitle>
          <AlertDialogDescription>
            Creates an IAM role trusted by the EKS control plane and attaches
            the AmazonEKSClusterPolicy managed policy.
          </AlertDialogDescription>
        </AlertDialogHeader>

        <Field>
          <FieldTitle>
            <label htmlFor="eks-role-name">Role name</label>
          </FieldTitle>
          <Input
            id="eks-role-name"
            onChange={(e) => setRoleName(e.target.value)}
            placeholder="my-cluster-eks-cluster-role"
            value={roleName}
          />
        </Field>

        <Field>
          <FieldTitle>Trust policy (JSON)</FieldTitle>
          <PolicyDocumentViewer
            document={EKS_CLUSTER_ASSUME_ROLE_POLICY_DOCUMENT}
          />
        </Field>

        <Field>
          <FieldTitle>Attached managed policy</FieldTitle>
          <code className="block rounded-md border bg-muted p-2 font-mono text-sm break-all">
            {AMAZON_EKS_CLUSTER_POLICY_ARN}
          </code>
          <p className="text-sm text-muted-foreground">
            Grants the EKS control plane the permissions it needs to manage the
            cluster.
          </p>
        </Field>

        {formError && <p className="text-sm text-destructive">{formError}</p>}

        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            disabled={isPending || !roleName.trim()}
            onClick={handleCreate}
          >
            {isPending ? "Creating…" : "Create role"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
