import { zodResolver } from "@hookform/resolvers/zod"
import { useSuspenseQuery } from "@tanstack/react-query"
import { useState } from "react"
import {
  type DeepPartialSkipArrayKey,
  useForm,
  useWatch,
} from "react-hook-form"

import {
  CliCommandPanel,
  type CliCommand,
  cliPlaceholder,
  commandFlag,
  optionalFlag,
  shellSingleQuote,
} from "@/components/cli-command-panel"
import { PanelSection } from "@/components/detail-panel"
import { FormActions } from "@/components/form-actions"
import { SelectField, TextField } from "@/components/form-fields"
import { FormLock } from "@/components/form-lock"
import { PickerNoticeText } from "@/components/picker-notice"
import { Field, FieldDescription, FieldTitle } from "@/components/ui/field"
import { VpcSelectField } from "@/components/vpc-select-field"
import {
  compactPolicy,
  FULL_ACCESS_POLICY,
  nameTagSpecificationArg,
} from "@/lib/vpc-endpoints"
import { useCreateVpcEndpoint } from "@/mutations/ec2"
import {
  ec2GatewayEndpointServicesQueryOptions,
  ec2VpcsQueryOptions,
} from "@/queries/ec2"
import {
  type CreateVpcEndpointFormData,
  createVpcEndpointSchema,
} from "@/types/ec2"

import { PolicyFields } from "./policy-fields"
import { RouteTablePicker } from "./route-table-picker"

interface CreateVpcEndpointPageProps {
  // Called with the new resource's ID once created, or with nothing on cancel.
  onClose: (id?: string) => void
}

export function CreateVpcEndpointPage({ onClose }: CreateVpcEndpointPageProps) {
  const { data: vpcsData } = useSuspenseQuery(ec2VpcsQueryOptions)

  const { data: servicesData } = useSuspenseQuery(
    ec2GatewayEndpointServicesQueryOptions,
  )

  const createMutation = useCreateVpcEndpoint()
  const vpcs = vpcsData.Vpcs ?? []
  const services = servicesData.ServiceDetails ?? []

  const {
    control,
    handleSubmit,
    register,
    setValue,
    formState: { errors, isSubmitting },
  } = useForm<CreateVpcEndpointFormData>({
    resolver: zodResolver(createVpcEndpointSchema),
    defaultValues: {
      name: "",
      // The one service there is needs no choosing.
      serviceName:
        services.length === 1 ? (services[0]?.ServiceName ?? "") : "",
      vpcId: "",
      routeTableIds: [],
      policy: "full",
      policyDocument: FULL_ACCESS_POLICY,
    },
  })

  const values = useWatch({ control })
  const [attempt, setAttempt] = useState<{ key: string; token: string }>()

  const service = services.find((s) => s.ServiceName === values.serviceName)
  // A service that takes a policy is offered only once the gate enforces it.
  const policySupported = service?.VpcEndpointPolicySupported === true

  const onSubmit = async (data: CreateVpcEndpointFormData) => {
    const request = {
      vpcId: data.vpcId,
      serviceName: data.serviceName,
      routeTableIds: data.routeTableIds,
      policyDocument:
        policySupported && data.policy === "custom"
          ? compactPolicy(data.policyDocument)
          : undefined,
      name: data.name?.trim() ?? undefined,
    }

    // A retry of the same request reuses its token; a changed one gets a new one.
    const key = JSON.stringify(request)

    const clientToken =
      attempt?.key === key ? attempt.token : crypto.randomUUID()

    setAttempt({ key, token: clientToken })

    let result

    try {
      result = await createMutation.mutateAsync({ ...request, clientToken })
    } catch {
      // Surfaced beside the submit button through the mutation's error.
      return
    }

    onClose(result.VpcEndpoint?.VpcEndpointId)
  }

  return (
    <form onSubmit={handleSubmit(onSubmit)}>
      <FormLock
        className="space-y-6"
        locked={createMutation.isPending || isSubmitting}
      >
        <TextField
          id="name"
          label="Name (optional)"
          placeholder="my-s3-endpoint"
          {...register("name")}
        />

        <PanelSection title="Service">
          <SelectField
            control={control}
            description="A gateway endpoint adds a route to the service's prefix list in each chosen route table."
            id="serviceName"
            label="Service"
            name="serviceName"
            notice={
              services.length === 0
                ? {
                    tone: "muted",
                    text: "This formation does not offer a gateway endpoint service yet.",
                  }
                : undefined
            }
            options={services.map((s) => ({
              value: s.ServiceName ?? "",
              label: s.ServiceName ?? "",
              detail: " · Gateway",
            }))}
            placeholder="Select a service"
          />
        </PanelSection>

        <PanelSection title="Network">
          <div className="space-y-6">
            <VpcSelectField
              control={control}
              id="vpcId"
              name="vpcId"
              onValueChange={(vpcId) => {
                // A route table belongs to one VPC, so a new VPC clears them.
                setValue("vpcId", vpcId, { shouldValidate: true })
                setValue("routeTableIds", [])
              }}
              placeholder="Select a VPC"
              vpcs={vpcs}
            />

            <Field>
              <FieldTitle>Route tables</FieldTitle>
              {values.vpcId ? (
                <RouteTablePicker
                  onChange={(next) => {
                    setValue("routeTableIds", next)
                  }}
                  selected={(values.routeTableIds ?? []).filter(
                    (id): id is string => id !== undefined,
                  )}
                  vpcId={values.vpcId}
                />
              ) : (
                <PickerNoticeText
                  notice={{
                    tone: "muted",
                    text: "Select a VPC to choose its route tables.",
                  }}
                />
              )}
              <FieldDescription>
                Subnets that use a chosen table reach the service without an
                internet or NAT gateway.
              </FieldDescription>
            </Field>
          </div>
        </PanelSection>

        {policySupported && (
          <PanelSection title="Policy">
            <div className="space-y-6">
              <PolicyFields
                document={values.policyDocument ?? ""}
                error={errors.policyDocument}
                mode={values.policy ?? "full"}
                onDocumentChange={(document) => {
                  setValue("policyDocument", document, {
                    shouldValidate: true,
                  })
                }}
                onModeChange={(mode) => {
                  setValue("policy", mode, { shouldValidate: true })
                }}
              />
            </div>
          </PanelSection>
        )}

        <CliCommandPanel
          commands={buildCreateVpcEndpointCommands(values, policySupported)}
        />

        <FormActions
          error={createMutation.error}
          errorMsg="Failed to create endpoint"
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={() => {
            onClose()
          }}
          pendingLabel="Creating…"
          submitDisabled={services.length === 0}
          submitLabel="Create endpoint"
        />
      </FormLock>
    </form>
  )
}

function buildCreateVpcEndpointCommands(
  values: DeepPartialSkipArrayKey<CreateVpcEndpointFormData>,
  policySupported: boolean,
): CliCommand[] {
  const tagName = values.name?.trim()

  const tables = (values.routeTableIds ?? []).filter(
    (id): id is string => id !== undefined,
  )

  const custom = policySupported && values.policy === "custom"

  return [
    {
      label: "Create endpoint",
      parts: [
        {
          type: "bin",
          value: "AWS_PROFILE=spinifex aws ec2 create-vpc-endpoint",
        },
        ...commandFlag("\\\n  --vpc-endpoint-type", "Gateway"),
        ...commandFlag(
          "\\\n  --vpc-id",
          cliPlaceholder(values.vpcId ?? "", "VpcId"),
        ),
        ...commandFlag(
          "\\\n  --service-name",
          cliPlaceholder(values.serviceName ?? "", "ServiceName"),
        ),
        ...optionalFlag("\\\n  --route-table-ids", tables.join(" ")),
        ...optionalFlag(
          "\\\n  --policy-document",
          custom
            ? shellSingleQuote(compactPolicy(values.policyDocument ?? ""))
            : undefined,
        ),
        ...optionalFlag(
          "\\\n  --tag-specifications",
          tagName ? nameTagSpecificationArg(tagName) : undefined,
        ),
      ],
    },
  ]
}
