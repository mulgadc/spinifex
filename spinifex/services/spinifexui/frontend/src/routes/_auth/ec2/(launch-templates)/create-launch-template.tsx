import type { RequestLaunchTemplateData } from "@aws-sdk/client-ec2"
import { zodResolver } from "@hookform/resolvers/zod"
import { createFileRoute, useNavigate } from "@tanstack/react-router"
import { useForm } from "react-hook-form"

import { BackLink } from "@/components/back-link"
import {
  CliCommandPanel,
  type CliCommand,
} from "@/components/cli-command-panel"
import { ErrorBanner } from "@/components/error-banner"
import { FormActions } from "@/components/form-actions"
import { PageHeading } from "@/components/page-heading"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  buildLaunchTemplateData,
  useCreateLaunchTemplate,
} from "@/mutations/ec2"
import {
  ec2ImagesQueryOptions,
  ec2InstanceTypesQueryOptions,
  ec2KeyPairsQueryOptions,
  ec2SecurityGroupsQueryOptions,
  ec2SubnetsQueryOptions,
} from "@/queries/ec2"
import {
  type LaunchTemplateFormData,
  createLaunchTemplateSchema,
} from "@/types/ec2"

import { LaunchTemplateDataFields } from "./-components/launch-template-data-fields"

export const Route = createFileRoute(
  "/_auth/ec2/(launch-templates)/create-launch-template",
)({
  loader: async ({ context }) => {
    await Promise.all([
      context.queryClient.ensureQueryData(ec2ImagesQueryOptions),
      context.queryClient.ensureQueryData(ec2InstanceTypesQueryOptions),
      context.queryClient.ensureQueryData(ec2KeyPairsQueryOptions),
      context.queryClient.ensureQueryData(ec2SubnetsQueryOptions),
      context.queryClient.ensureQueryData(ec2SecurityGroupsQueryOptions),
    ])
  },
  head: () => ({
    meta: [
      {
        title: "Create Launch Template | EC2 | Mulga",
      },
    ],
  }),
  component: CreateLaunchTemplate,
})

function CreateLaunchTemplate() {
  const navigate = useNavigate()
  const createMutation = useCreateLaunchTemplate()

  const {
    control,
    handleSubmit,
    register,
    watch,
    formState: { errors, isSubmitting },
  } = useForm<LaunchTemplateFormData>({
    resolver: zodResolver(createLaunchTemplateSchema),
    defaultValues: {
      launchTemplateName: "",
      versionDescription: "",
      imageId: "",
      instanceType: "",
      securityGroupIds: [],
      userData: "",
    },
  })

  const values = watch()

  const onSubmit = async (data: LaunchTemplateFormData) => {
    const result = await createMutation.mutateAsync(data)
    const id = result.LaunchTemplate?.LaunchTemplateId
    if (!id) {
      navigate({ to: "/ec2/describe-launch-templates" })
      return
    }
    navigate({ to: "/ec2/describe-launch-templates/$id", params: { id } })
  }

  return (
    <>
      <BackLink to="/ec2/describe-launch-templates">
        Back to launch templates
      </BackLink>

      <PageHeading title="Create Launch Template" />

      {createMutation.error && (
        <ErrorBanner
          error={createMutation.error}
          msg="Failed to create launch template"
        />
      )}

      <form className="max-w-4xl space-y-6" onSubmit={handleSubmit(onSubmit)}>
        <Field>
          <FieldTitle>
            <label htmlFor="launchTemplateName">Name</label>
          </FieldTitle>
          <Input
            aria-invalid={!!errors.launchTemplateName}
            id="launchTemplateName"
            placeholder="my-launch-template"
            {...register("launchTemplateName")}
          />
          <FieldError errors={[errors.launchTemplateName]} />
        </Field>

        <Field>
          <FieldTitle>
            <label htmlFor="versionDescription">Version Description</label>
          </FieldTitle>
          <Input
            id="versionDescription"
            placeholder="Initial version"
            {...register("versionDescription")}
          />
        </Field>

        <LaunchTemplateDataFields
          control={control}
          errors={errors}
          register={register}
        />

        <CliCommandPanel
          commands={buildCreateLaunchTemplateCommands(
            values.launchTemplateName ?? "",
            values.versionDescription ?? "",
            buildLaunchTemplateData(values),
          )}
        />

        <FormActions
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={async () =>
            await navigate({ to: "/ec2/describe-launch-templates" })
          }
          pendingLabel="Creating…"
          submitLabel="Create Launch Template"
        />
      </form>
    </>
  )
}

function shellSingleQuote(value: string): string {
  return `'${value.replaceAll("'", `'"'"'`)}'`
}

function buildCreateLaunchTemplateCommands(
  name: string,
  versionDescription: string,
  data: RequestLaunchTemplateData,
): CliCommand[] {
  const parts: CliCommand["parts"] = [
    {
      type: "bin",
      value: "AWS_PROFILE=spinifex aws ec2 create-launch-template",
    },
    { type: "flag", value: " \\\n  --launch-template-name" },
    { type: "value", value: ` ${name ? shellSingleQuote(name) : "<name>"}` },
  ]
  if (versionDescription) {
    parts.push(
      { type: "flag", value: " \\\n  --version-description" },
      { type: "value", value: ` ${shellSingleQuote(versionDescription)}` },
    )
  }
  parts.push(
    { type: "flag", value: " \\\n  --launch-template-data" },
    { type: "value", value: ` ${shellSingleQuote(JSON.stringify(data))}` },
  )
  return [{ label: "Create Launch Template", parts }]
}
