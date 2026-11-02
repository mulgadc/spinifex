import { LoaderCircle } from "lucide-react"
import { useState } from "react"

import { Button } from "@/components/ui/button"
import { Field, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { useAttachPolicy, useCreateRole } from "@/mutations/iam"
import { PolicyDocumentViewer } from "@/routes/_auth/iam/-components/policy-document-viewer"
import {
  AMAZON_EKS_CLUSTER_POLICY_ARN,
  EKS_CLUSTER_ASSUME_ROLE_POLICY_DOCUMENT,
} from "@/types/eks"

interface CreateClusterRoleSectionProps {
  clusterName: string
  onCancel: () => void
  onCreated: (roleArn: string) => void
}

function defaultRoleName(clusterName: string): string {
  const base = clusterName.trim() || "cluster"

  return `${base}-eks-cluster-role`
}

// Sits inside the create-cluster form, so its buttons are type="button" and
// Enter in the name field creates the role rather than submitting the cluster.
export function CreateClusterRoleSection({
  clusterName,
  onCancel,
  onCreated,
}: CreateClusterRoleSectionProps) {
  const createRole = useCreateRole()
  const attachPolicy = useAttachPolicy("role")
  const [roleName, setRoleName] = useState(() => defaultRoleName(clusterName))
  const [formError, setFormError] = useState<string | null>(null)

  const isPending = createRole.isPending || attachPolicy.isPending
  const canCreate = !isPending && roleName.trim() !== ""

  async function handleCreate() {
    setFormError(null)
    const name = roleName.trim()

    try {
      const result = await createRole.mutateAsync({
        roleName: name,
        assumeRolePolicyDocument: EKS_CLUSTER_ASSUME_ROLE_POLICY_DOCUMENT,
      })

      await attachPolicy.mutateAsync({
        name,
        policyArn: AMAZON_EKS_CLUSTER_POLICY_ARN,
      })
      const arn = result.Role?.Arn

      if (!arn) {
        setFormError("Role was created but no ARN was returned.")

        return
      }

      onCreated(arn)
    } catch (error) {
      setFormError(
        error instanceof Error ? error.message : "Failed to create role.",
      )
    }
  }

  return (
    <section
      aria-label="Create cluster IAM role"
      className="space-y-4 rounded-lg border bg-background/40 p-4"
    >
      <div className="space-y-1">
        <h3 className="text-sm font-semibold">Create cluster IAM role</h3>
        <p className="text-xs text-muted-foreground">
          Creates an IAM role trusted by the EKS control plane and attaches the
          AmazonEKSClusterPolicy managed policy.
        </p>
      </div>

      <Field>
        <FieldTitle>
          <label htmlFor="eks-role-name">Role name</label>
        </FieldTitle>
        <Input
          disabled={isPending}
          id="eks-role-name"
          onChange={(e) => {
            setRoleName(e.target.value)
          }}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              e.preventDefault()

              if (canCreate) {
                void handleCreate()
              }
            }
          }}
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
      </Field>

      {formError && <p className="text-sm text-destructive">{formError}</p>}

      <div className="flex gap-2">
        <Button
          disabled={isPending}
          onClick={onCancel}
          size="sm"
          type="button"
          variant="outline"
        >
          Cancel
        </Button>
        <Button
          aria-busy={isPending}
          disabled={!canCreate}
          onClick={() => {
            void handleCreate()
          }}
          size="sm"
          type="button"
        >
          {isPending && (
            <LoaderCircle
              aria-hidden
              className="animate-spin motion-reduce:animate-none"
              data-icon="inline-start"
            />
          )}
          {isPending ? "Creating…" : "Create role"}
        </Button>
      </div>
    </section>
  )
}
