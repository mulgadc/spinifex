import type { VpcEndpoint } from "@aws-sdk/client-ec2"
import { useState } from "react"

import {
  CliCommandPanel,
  type CliCommand,
  commandFlag,
  optionalFlag,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { FormLock } from "@/components/form-lock"
import { FormPanel } from "@/components/form-panel"
import { Field, FieldDescription, FieldTitle } from "@/components/ui/field"
import { routeTableChanges } from "@/lib/vpc-endpoints"
import { useModifyVpcEndpoint } from "@/mutations/ec2"

import { RouteTablePicker } from "./route-table-picker"

function buildCommands(
  endpointId: string,
  add: string[],
  remove: string[],
): CliCommand[] {
  return [
    {
      label: "Modify endpoint",
      parts: [
        {
          type: "bin",
          value: "AWS_PROFILE=spinifex aws ec2 modify-vpc-endpoint",
        },
        ...commandFlag("\\\n  --vpc-endpoint-id", endpointId),
        ...optionalFlag("\\\n  --add-route-table-ids", add.join(" ")),
        ...optionalFlag("\\\n  --remove-route-table-ids", remove.join(" ")),
      ],
    },
  ]
}

// Chooses the route tables an endpoint serves, sent as one modify that adds
// and removes only the difference.
export function ManageRouteTablesForm({
  endpoint,
  onClose,
}: {
  endpoint: VpcEndpoint
  onClose: () => void
}) {
  const id = endpoint.VpcEndpointId ?? ""
  const current = endpoint.RouteTableIds ?? []
  const [selected, setSelected] = useState<string[]>(current)
  const modify = useModifyVpcEndpoint()
  const { add, remove } = routeTableChanges(current, selected)

  async function onSubmit() {
    try {
      await modify.mutateAsync({
        vpcEndpointId: id,
        addRouteTableIds: add,
        removeRouteTableIds: remove,
      })
    } catch {
      // Shown beside the submit button through the mutation's error.
      return
    }

    onClose()
  }

  return (
    <FormPanel onClose={onClose} subtitle={id} title="Manage route tables">
      <form
        onSubmit={(event) => {
          event.preventDefault()
          void onSubmit()
        }}
      >
        <FormLock className="space-y-6" locked={modify.isPending}>
          <Field>
            <FieldTitle>Route tables</FieldTitle>
            <RouteTablePicker
              onChange={setSelected}
              selected={selected}
              vpcId={endpoint.VpcId ?? ""}
            />
            <FieldDescription>
              A removed table loses its route to the service&apos;s prefix list,
              and its subnets reach the service only by their other routes.
            </FieldDescription>
          </Field>
          <CliCommandPanel commands={buildCommands(id, add, remove)} />
          <FormActions
            error={modify.error}
            errorMsg="Failed to change route tables"
            isPending={modify.isPending}
            isSubmitting={false}
            onCancel={onClose}
            pendingLabel="Saving…"
            submitDisabled={add.length === 0 && remove.length === 0}
            submitLabel="Save route tables"
          />
        </FormLock>
      </form>
    </FormPanel>
  )
}
