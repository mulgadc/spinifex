import type { RequestLaunchTemplateData } from "@aws-sdk/client-ec2"
import { zodResolver } from "@hookform/resolvers/zod"
import { useForm, useWatch } from "react-hook-form"

import {
  CliCommandPanel,
  type CliCommand,
  commandFlag,
  optionalFlag,
  shellSingleQuote,
} from "@/components/cli-command-panel"
import { FormActions } from "@/components/form-actions"
import { FormLock } from "@/components/form-lock"
import { Field, FieldError, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  buildLaunchTemplateData,
  useCreateLaunchTemplate,
} from "@/mutations/ec2"
import {
  type LaunchTemplateFormData,
  createLaunchTemplateSchema,
} from "@/types/ec2"

import { LaunchTemplateDataFields } from "./launch-template-data-fields"

interface CreateLaunchTemplatePageProps {
  // Called with the new resource's ID once created, or with nothing on cancel.
  onClose: (id?: string) => void
}

export function CreateLaunchTemplatePage({
  onClose,
}: CreateLaunchTemplatePageProps) {
  const createMutation = useCreateLaunchTemplate()

  const {
    control,
    handleSubmit,
    register,
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

  const values = useWatch({ control })

  const onSubmit = async (data: LaunchTemplateFormData) => {
    let result

    try {
      result = await createMutation.mutateAsync(data)
    } catch {
      // Surfaced beside the submit button through the mutation's error.
      return
    }

    onClose(result.LaunchTemplate?.LaunchTemplateId)
  }

  return (
    <form onSubmit={handleSubmit(onSubmit)}>
      <FormLock
        className="space-y-6"
        locked={createMutation.isPending || isSubmitting}
      >
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
          error={createMutation.error}
          errorMsg="Failed to create launch template"
          isPending={createMutation.isPending}
          isSubmitting={isSubmitting}
          onCancel={() => {
            onClose()
          }}
          pendingLabel="Creating…"
          submitLabel="Create Launch Template"
        />
      </FormLock>
    </form>
  )
}

function buildCreateLaunchTemplateCommands(
  name: string,
  versionDescription: string,
  data: RequestLaunchTemplateData,
): CliCommand[] {
  return [
    {
      label: "Create Launch Template",
      parts: [
        {
          type: "bin",
          value: "AWS_PROFILE=spinifex aws ec2 create-launch-template",
        },
        ...commandFlag(
          "\\\n  --launch-template-name",
          name ? shellSingleQuote(name) : "<name>",
        ),
        ...optionalFlag(
          "\\\n  --version-description",
          versionDescription && shellSingleQuote(versionDescription),
        ),
        ...commandFlag(
          "\\\n  --launch-template-data",
          shellSingleQuote(JSON.stringify(data)),
        ),
      ],
    },
  ]
}
