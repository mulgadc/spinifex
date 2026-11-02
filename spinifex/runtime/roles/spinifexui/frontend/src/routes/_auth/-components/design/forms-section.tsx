import { zodResolver } from "@hookform/resolvers/zod"
import { Plus } from "lucide-react"
import { useState } from "react"
import { Controller, useForm, useWatch } from "react-hook-form"
import { z } from "zod"

import {
  CliCommandPanel,
  commandFlag,
  optionalFlag,
} from "@/components/cli-command-panel"
import { CreatePanel } from "@/components/create-panel"
import { PanelSection } from "@/components/detail-panel"
import { FormActions } from "@/components/form-actions"
import { SelectField, selectOptions, TextField } from "@/components/form-fields"
import { FormLock } from "@/components/form-lock"
import { FormPanel } from "@/components/form-panel"
import { SwitchList } from "@/components/switch-list"
import { TagsFieldArray } from "@/components/tags-field-array"
import { Button } from "@/components/ui/button"
import { Field, FieldTitle } from "@/components/ui/field"
import { Label } from "@/components/ui/label"
import { Switch } from "@/components/ui/switch"

import { CatalogueSection, Specimen, unwired } from "./catalogue-section"

const SUBNETS = [
  {
    id: "subnet-0a1b2c3d",
    label: "subnet-0a1b2c3d",
    detail: "az1 · 10.0.1.0/24",
  },
  {
    id: "subnet-0e4f5a6b",
    label: "subnet-0e4f5a6b",
    detail: "az1 · 10.0.2.0/24",
  },
  {
    id: "subnet-0c7d8e9f",
    label: "subnet-0c7d8e9f",
    detail: "az1 · 10.0.3.0/24",
  },
]

const sampleSchema = z.object({
  name: z.string().min(1, "Enter a name"),
  scheme: z.enum(["internet-facing", "internal"]),
  deletionProtection: z.boolean(),
  subnetIds: z.array(z.string()).min(1, "Pick at least one subnet"),
  tags: z.array(z.object({ Key: z.string(), Value: z.string() })),
})

type SampleForm = z.infer<typeof sampleSchema>

// Stands in for a mutation: it takes a moment and then refuses, so the
// spinner, the locked form and the error beside the button can all be seen.
async function refuseSlowly(): Promise<never> {
  // oxlint-disable-next-line promise/avoid-new -- a stand-in for a request
  await new Promise((resolve) => {
    window.setTimeout(resolve, 1500)
  })
  const error = new Error("The sample gateway refused the request.")
  error.name = "InvalidParameterValue"
  throw error
}

// The reference create form: fields in the order AWS asks for them, switches
// for settings and pickers, the CLI equivalent, then the actions.
function SampleCreateForm({ onClose }: { onClose: () => void }) {
  const [error, setError] = useState<Error | null>(null)

  const form = useForm<SampleForm>({
    resolver: zodResolver(sampleSchema),
    defaultValues: {
      name: "",
      scheme: "internet-facing",
      deletionProtection: false,
      subnetIds: [],
      tags: [],
    },
  })

  const {
    control,
    handleSubmit,
    register,
    setValue,
    formState: { errors, isSubmitting },
  } = form

  const values = useWatch({ control })

  async function onSubmit() {
    setError(null)

    try {
      await refuseSlowly()
    } catch (error_) {
      setError(error_ instanceof Error ? error_ : null)
    }
  }

  return (
    <form onSubmit={handleSubmit(onSubmit)}>
      <FormLock className="space-y-6" locked={isSubmitting}>
        <TextField
          description="Up to 32 characters."
          error={errors.name}
          id="sample-name"
          label="Name"
          placeholder="web-lb"
          {...register("name")}
        />
        <SelectField
          control={control}
          id="sample-scheme"
          info="Internet-facing takes traffic from outside the VPC; internal only from inside it."
          label="Scheme"
          name="scheme"
          options={selectOptions(["internet-facing", "internal"])}
        />
        <Controller
          control={control}
          name="deletionProtection"
          render={({ field }) => (
            <div className="flex items-center gap-2">
              <Switch
                checked={field.value}
                id="sample-protect"
                onCheckedChange={field.onChange}
              />
              <Label htmlFor="sample-protect">Deletion protection</Label>
            </div>
          )}
        />
        <Field>
          <FieldTitle>Subnets</FieldTitle>
          <SwitchList
            emptyText="No subnets in this VPC."
            itemLabel="Subnet"
            items={SUBNETS}
            onChange={(next) => {
              setValue("subnetIds", next, { shouldValidate: true })
            }}
            selected={values.subnetIds ?? []}
          />
          {errors.subnetIds && (
            <p className="text-xs text-destructive">
              {errors.subnetIds.message}
            </p>
          )}
        </Field>
        <TagsFieldArray control={control} name="tags" />
        <CliCommandPanel
          commands={[
            {
              label: "Create load balancer",
              parts: [
                { type: "bin", value: "aws elbv2 create-load-balancer" },
                ...commandFlag("--name", values.name || "web-lb"),
                ...optionalFlag("--scheme", values.scheme),
                ...commandFlag(
                  "--subnets",
                  (values.subnetIds ?? []).join(" ") || "subnet-…",
                ),
              ],
            },
          ]}
        />
        <FormActions
          error={error}
          errorMsg="Failed to create load balancer"
          isPending={false}
          isSubmitting={isSubmitting}
          onCancel={onClose}
          pendingLabel="Creating…"
          submitLabel="Create load balancer"
        />
      </FormLock>
    </form>
  )
}

export function FormsSection() {
  const [panelOpen, setPanelOpen] = useState(false)
  const [formOpen, setFormOpen] = useState(false)
  const [picked, setPicked] = useState(["subnet-0a1b2c3d"])

  return (
    <CatalogueSection
      description="Every create flow opens in the side panel, and a form opened from a resource's panel lies over that panel. Submitting spins the button in place and locks the form; a failure shows beside the button and releases it."
      title="Forms"
    >
      <Specimen label="Create flow">
        <Button
          onClick={() => {
            setPanelOpen(true)
          }}
        >
          <Plus data-icon="inline-start" />
          Open in the create panel
        </Button>
      </Specimen>
      <Specimen label="Form from a resource's panel">
        <Button
          onClick={() => {
            setFormOpen(true)
          }}
          variant="outline"
        >
          Open a form panel
        </Button>
      </Specimen>
      <Specimen label="Create form, submit fails after 1.5 s">
        <div className="w-full max-w-xl">
          <PanelSection title="Sample">
            <SampleCreateForm
              onClose={() => {
                setPanelOpen(false)
              }}
            />
          </PanelSection>
        </div>
      </Specimen>
      <Specimen label="Submit states">
        <FormActions
          isPending={false}
          isSubmitting={false}
          onCancel={unwired}
          pendingLabel="Creating…"
          submitLabel="Create"
        />
        <FormActions
          isPending
          isSubmitting={false}
          onCancel={unwired}
          pendingLabel="Creating…"
          submitLabel="Create"
        />
      </Specimen>
      <Specimen label="Multi-select picker">
        <div className="w-full max-w-md">
          <SwitchList
            emptyText="No subnets in this VPC."
            itemLabel="Subnet"
            items={SUBNETS}
            onChange={setPicked}
            selected={picked}
          />
        </div>
        <div className="w-full max-w-md">
          <SwitchList
            emptyText="No security groups in the resolved VPC."
            itemLabel="Security group"
            items={[]}
            onChange={setPicked}
            selected={[]}
          />
        </div>
      </Specimen>
      {formOpen && (
        <FormPanel
          onClose={() => {
            setFormOpen(false)
          }}
          subtitle="vol-0a1b2c3d4e5f60718"
          title="Attach volume"
        >
          <SampleCreateForm
            onClose={() => {
              setFormOpen(false)
            }}
          />
        </FormPanel>
      )}
      {panelOpen && (
        <CreatePanel
          onClose={() => {
            setPanelOpen(false)
          }}
          title="Create load balancer"
        >
          <SampleCreateForm
            onClose={() => {
              setPanelOpen(false)
            }}
          />
        </CreatePanel>
      )}
    </CatalogueSection>
  )
}
