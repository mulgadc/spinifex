import type { VpcEndpoint } from "@aws-sdk/client-ec2"
import { useState } from "react"

import {
  CliCommandPanel,
  type CliCommand,
  commandFlag,
  shellSingleQuote,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { FormLock } from "@/components/form-lock"
import { FormPanel } from "@/components/form-panel"
import { formatJson, isValidJson } from "@/lib/json"
import {
  compactPolicy,
  FULL_ACCESS_POLICY,
  isFullAccessPolicy,
} from "@/lib/vpc-endpoints"
import { useModifyVpcEndpoint } from "@/mutations/ec2"

import { type PolicyMode, PolicyFields } from "./policy-fields"

function buildCommands(
  endpointId: string,
  mode: PolicyMode,
  document: string,
): CliCommand[] {
  const compact = compactPolicy(document)

  return [
    {
      label: "Modify endpoint",
      parts: [
        {
          type: "bin",
          value: "AWS_PROFILE=spinifex aws ec2 modify-vpc-endpoint",
        },
        ...commandFlag("\\\n  --vpc-endpoint-id", endpointId),
        ...(mode === "full"
          ? [{ type: "flag" as const, value: " \\\n  --reset-policy" }]
          : commandFlag("\\\n  --policy-document", shellSingleQuote(compact))),
      ],
    },
  ]
}

// Sets a custom policy, or resets the endpoint to full access as AWS does.
export function EditPolicyForm({
  endpoint,
  onClose,
}: {
  endpoint: VpcEndpoint
  onClose: () => void
}) {
  const id = endpoint.VpcEndpointId ?? ""
  const stored = endpoint.PolicyDocument

  const [mode, setMode] = useState<PolicyMode>(
    isFullAccessPolicy(stored) ? "full" : "custom",
  )

  const [document, setDocument] = useState(
    stored ? (formatJson(stored) ?? stored) : FULL_ACCESS_POLICY,
  )

  const modify = useModifyVpcEndpoint()
  const invalid = mode === "custom" && !isValidJson(document)
  // Resetting a policy that is already full access changes nothing.
  const unchanged = mode === "full" && isFullAccessPolicy(stored)

  async function onSubmit() {
    try {
      await modify.mutateAsync(
        mode === "full"
          ? { vpcEndpointId: id, resetPolicy: true }
          : { vpcEndpointId: id, policyDocument: compactPolicy(document) },
      )
    } catch {
      // Shown beside the submit button through the mutation's error.
      return
    }

    onClose()
  }

  return (
    <FormPanel onClose={onClose} subtitle={id} title="Edit policy">
      <form
        onSubmit={(event) => {
          event.preventDefault()
          void onSubmit()
        }}
      >
        <FormLock className="space-y-6" locked={modify.isPending}>
          <PolicyFields
            document={document}
            error={
              invalid ? { message: "Policy must be valid JSON" } : undefined
            }
            mode={mode}
            onDocumentChange={setDocument}
            onModeChange={setMode}
          />
          <CliCommandPanel commands={buildCommands(id, mode, document)} />
          <FormActions
            error={modify.error}
            errorMsg="Failed to save policy"
            isPending={modify.isPending}
            isSubmitting={false}
            onCancel={onClose}
            pendingLabel="Saving…"
            submitDisabled={invalid || unchanged}
            submitLabel="Save policy"
          />
        </FormLock>
      </form>
    </FormPanel>
  )
}
