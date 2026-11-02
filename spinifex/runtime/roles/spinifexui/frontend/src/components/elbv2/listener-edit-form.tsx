import type { Listener } from "@aws-sdk/client-elastic-load-balancing-v2"
import { zodResolver } from "@hookform/resolvers/zod"
import type { SubmitEvent } from "react"
import { useForm, useWatch } from "react-hook-form"

import {
  type CliCommand,
  CliCommandPanel,
  cliPlaceholder,
  commandFlag,
  optionalFlag,
} from "@/components/cli-command-panel"
import { ListenerForm } from "@/components/elbv2/listener-form"
import { FormActions } from "@/components/form-actions"
import { FormLock } from "@/components/form-lock"
import { FormPanel } from "@/components/form-panel"
import { FieldDescription } from "@/components/ui/field"
import {
  type CreateListenerFormData,
  createListenerSchema,
  defaultProtocolForType,
  isSecureProtocol,
  type LbType,
  type ListenerProtocolValue,
} from "@/types/elbv2"

// NLB listeners default to TCP, ALB to HTTP — both on port 80.
function listenerDefaults(lbType: LbType): CreateListenerFormData {
  return {
    protocol: defaultProtocolForType(lbType),
    port: 80,
    defaultTargetGroupArn: "",
  }
}

// Projects an existing listener onto the form shape. TLS fields are dropped for
// plaintext protocols so a downgrade does not carry a stale certificate.
function listenerFormValues(
  listener: Listener,
  protocols: readonly ListenerProtocolValue[],
): CreateListenerFormData {
  const protocol: ListenerProtocolValue =
    protocols.find((p) => p === listener.Protocol) ?? protocols[0] ?? "HTTP"

  const secure = isSecureProtocol(protocol)

  return {
    protocol,
    port: listener.Port ?? 80,
    defaultTargetGroupArn: listener.DefaultActions?.[0]?.TargetGroupArn ?? "",
    certificateArn: secure
      ? listener.Certificates?.[0]?.CertificateArn
      : undefined,
    sslPolicy: protocol === "HTTPS" ? listener.SslPolicy : undefined,
  }
}

interface ListenerEditFormProps {
  loadBalancerArn: string
  lbType: LbType
  protocols: readonly ListenerProtocolValue[]
  targetGroups: React.ComponentProps<typeof ListenerForm>["targetGroups"]
  certificates: React.ComponentProps<typeof ListenerForm>["certificates"]
  sslPolicies: React.ComponentProps<typeof ListenerForm>["sslPolicies"]
  isPending: boolean
  error: Error | null
  onSubmit: (data: CreateListenerFormData) => Promise<void>
  onClose: () => void
  // Set to edit that listener; unset adds a new one.
  listener?: Listener
}

export function ListenerEditForm({
  loadBalancerArn,
  lbType,
  protocols,
  targetGroups,
  certificates,
  sslPolicies,
  isPending,
  error,
  onSubmit,
  onClose,
  listener,
}: ListenerEditFormProps) {
  const form = useForm<CreateListenerFormData>({
    resolver: zodResolver(createListenerSchema),
    defaultValues: listener
      ? listenerFormValues(listener, protocols)
      : listenerDefaults(lbType),
  })

  const values = useWatch({ control: form.control })
  const editing = listener !== undefined

  function handleSubmit(event: SubmitEvent<HTMLFormElement>) {
    event.stopPropagation()
    void form.handleSubmit(onSubmit)(event)
  }

  return (
    <FormPanel
      onClose={onClose}
      subtitle={listener ? `${listener.Protocol}:${listener.Port}` : undefined}
      title={editing ? "Edit listener" : "Add listener"}
    >
      <form onSubmit={handleSubmit}>
        <FormLock className="space-y-4" locked={isPending}>
          <FieldDescription>
            {editing
              ? "Update port, protocol, or default target group. Changes apply without dropping the listener."
              : "Listeners forward incoming traffic to a default target group."}
          </FieldDescription>
          <ListenerForm
            certificates={certificates}
            form={form}
            protocols={protocols}
            sslPolicies={sslPolicies}
            targetGroups={targetGroups}
          />
          <CliCommandPanel
            commands={buildListenerCommands(
              values,
              listener?.ListenerArn ?? loadBalancerArn,
              editing,
            )}
          />
          <FormActions
            error={error}
            errorMsg={
              editing
                ? "Failed to modify listener"
                : "Failed to create listener"
            }
            isPending={isPending}
            isSubmitting={form.formState.isSubmitting}
            onCancel={onClose}
            pendingLabel={editing ? "Saving…" : "Adding…"}
            submitLabel={editing ? "Save changes" : "Add listener"}
          />
        </FormLock>
      </form>
    </FormPanel>
  )
}

export function buildListenerCommands(
  values: Partial<CreateListenerFormData>,
  arn: string,
  editing: boolean,
): CliCommand[] {
  const secure = values.protocol ? isSecureProtocol(values.protocol) : false

  return [
    {
      label: editing ? "Modify listener" : "Create listener",
      parts: [
        {
          type: "bin",
          value: `AWS_PROFILE=spinifex aws elbv2 ${editing ? "modify-listener" : "create-listener"}`,
        },
        ...commandFlag(editing ? "--listener-arn" : "--load-balancer-arn", arn),
        ...optionalFlag("--protocol", values.protocol),
        ...optionalFlag("--port", values.port),
        ...commandFlag(
          "--default-actions",
          `Type=forward,TargetGroupArn=${cliPlaceholder(values.defaultTargetGroupArn ?? "", "TargetGroupArn")}`,
        ),
        ...optionalFlag(
          "--certificates",
          secure && values.certificateArn
            ? `CertificateArn=${values.certificateArn}`
            : undefined,
        ),
        ...optionalFlag(
          "--ssl-policy",
          values.protocol === "HTTPS" ? values.sslPolicy : undefined,
        ),
      ],
    },
  ]
}
