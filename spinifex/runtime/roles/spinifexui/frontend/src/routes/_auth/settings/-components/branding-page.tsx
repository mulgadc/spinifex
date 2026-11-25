import { zodResolver } from "@hookform/resolvers/zod"
import { useQuery } from "@tanstack/react-query"
import { useState } from "react"
import { type Control, useForm, useWatch } from "react-hook-form"

import { ColorField } from "@/components/color-field"
import { PanelSection } from "@/components/detail-panel"
import { ErrorBanner } from "@/components/error-banner"
import { FormActions } from "@/components/form-actions"
import { SelectField, TextField } from "@/components/form-fields"
import { FormLock } from "@/components/form-lock"
import { ImageUploadField } from "@/components/image-upload-field"
import { PageHeading } from "@/components/page-heading"
import { Button } from "@/components/ui/button"
import { DEFAULT_DOCS_URL, DEFAULT_PRODUCT_NAME } from "@/lib/branding"
import { isHexColor, type ThemeName, tokenHex } from "@/lib/theme-tokens"
import { usePutConsoleBranding } from "@/mutations/branding"
import { consoleBrandingQueryOptions } from "@/queries/branding"
import {
  BRANDING_IMAGE_TYPES,
  type BrandingFormData,
  brandingFormSchema,
  brandingFormValues,
  type BrandingImageChanges,
  type ImageChange,
  brandingRequest,
  type ConsoleBrandingResponse,
  type DocsLink,
  MAX_FAVICON_BYTES,
  MAX_LOGO_BYTES,
  MAX_PRODUCT_NAME_LENGTH,
  resetBrandingRequest,
} from "@/types/branding"

import { BrandingPreview } from "./branding-preview"
import { ResetBrandingDialog } from "./reset-branding-dialog"

const KEEP_ALL: BrandingImageChanges = {
  logo: { kind: "keep" },
  logoDark: { kind: "keep" },
  favicon: { kind: "keep" },
}

// The stored image the preview shows, or none once the operator removes it. A
// new upload cannot show, as the page's CSP allows only same-origin images.
function previewLogo(
  stored: ConsoleBrandingResponse,
  change: ImageChange,
  name: "logo" | "logo-dark",
): string | undefined {
  const present = name === "logo" ? stored.logo : stored.logoDark

  return present && change.kind === "keep"
    ? storedAsset(stored, name)
    : undefined
}

const DOCS_OPTIONS: { value: DocsLink; label: string }[] = [
  { value: "default", label: "Built-in documentation" },
  { value: "custom", label: "Your documentation" },
  { value: "hidden", label: "No Docs link" },
]

export function BrandingPage() {
  const query = useQuery(consoleBrandingQueryOptions)
  // Held here, so the result outlives the form, which starts again on each
  // new revision.
  const save = usePutConsoleBranding()
  const reset = usePutConsoleBranding()

  return (
    <>
      <PageHeading
        subtitle="The name, logos, colours and Docs link every user of this cluster sees, from the login page on."
        title="Branding"
      />
      {query.isPending && (
        <p className="text-sm text-muted-foreground">Loading the branding…</p>
      )}
      {query.error && <BrandingReadError error={query.error} />}
      {query.data && (
        <BrandingForm
          key={query.data.revision}
          onReset={async () => {
            await reset.mutateAsync(resetBrandingRequest())
          }}
          resetPending={reset.isPending}
          save={save}
          stored={query.data}
        />
      )}
      {(save.isSuccess || reset.isSuccess) && !save.isPending && (
        <output className="mt-4 block text-sm text-muted-foreground">
          Saved. Pages loaded from now on show it; reload this page to see it
          here.
        </output>
      )}
      {reset.error && (
        <ErrorBanner
          className="mt-4"
          error={reset.error}
          msg="Failed to reset the branding"
        />
      )}
    </>
  )
}

const GRANT_HELP =
  "It needs spinifex:GetConsoleBranding and spinifex:PutConsoleBranding. Sign in as the account's admin user, or make a principal for branding on a node with spx admin principal create <name> --grant GetConsoleBranding,PutConsoleBranding."

// The grant check comes first, so an operator whose principal predates these
// methods is told how to get them rather than shown a bare refusal.
function BrandingReadError({ error }: { error: Error }) {
  if (error.name !== "AccessDenied") {
    return <ErrorBanner error={error} msg="Failed to read the branding" />
  }

  return (
    <ErrorBanner
      error={new Error(GRANT_HELP)}
      msg="This principal may not read or change the branding"
    />
  )
}

interface BrandingFormProps {
  stored: ConsoleBrandingResponse
  save: ReturnType<typeof usePutConsoleBranding>
  onReset: () => Promise<void>
  resetPending: boolean
}

function storedAsset(
  stored: ConsoleBrandingResponse,
  name: "logo" | "logo-dark" | "favicon",
): string {
  return `/branding/${name}?rev=${stored.revision}`
}

function BrandingForm({
  stored,
  save,
  onReset,
  resetPending,
}: BrandingFormProps) {
  const [images, setImages] = useState(KEEP_ALL)
  const [confirmReset, setConfirmReset] = useState(false)

  const {
    control,
    formState: { errors, isSubmitting },
    handleSubmit,
    register,
    reset: resetForm,
  } = useForm<BrandingFormData>({
    resolver: zodResolver(brandingFormSchema),
    defaultValues: brandingFormValues(stored),
  })

  const values = useWatch({ control })
  const logo = previewLogo(stored, images.logo, "logo")
  const logoDark = previewLogo(stored, images.logoDark, "logo-dark") ?? logo
  const locked = save.isPending || isSubmitting || resetPending

  async function onSubmit(form: BrandingFormData) {
    try {
      await save.mutateAsync(brandingRequest(form, images))
    } catch {
      // Shown beside the button.
    }
  }

  return (
    <form onSubmit={handleSubmit(onSubmit)}>
      <FormLock className="max-w-4xl space-y-6" locked={locked}>
        <PanelSection title="Name and links">
          <div className="space-y-6">
            <TextField
              description={`Shown in the sidebar and on the login page. Empty uses ${DEFAULT_PRODUCT_NAME}.`}
              error={errors.productName}
              id="branding-product-name"
              label="Product name"
              maxLength={MAX_PRODUCT_NAME_LENGTH}
              placeholder={DEFAULT_PRODUCT_NAME}
              {...register("productName")}
            />
            <SelectField
              control={control}
              description={`The sidebar's Docs link. The built-in one goes to ${DEFAULT_DOCS_URL}.`}
              id="branding-docs"
              label="Docs link"
              name="docs"
              options={DOCS_OPTIONS}
            />
            {values.docs === "custom" && (
              <TextField
                error={errors.docsUrl}
                id="branding-docs-url"
                label="Documentation URL"
                placeholder="https://docs.example.com"
                type="url"
                {...register("docsUrl")}
              />
            )}
          </div>
        </PanelSection>

        <PanelSection title="Colours">
          <div className="space-y-6">
            <ThemeColorField control={control} theme="light" />
            <ThemeColorField control={control} theme="dark" />
          </div>
        </PanelSection>

        <PanelSection title="Images">
          <div className="space-y-6">
            <ImageUploadField
              accept={BRANDING_IMAGE_TYPES}
              description="Shown in the sidebar header. PNG, WebP or SVG, up to 128 KiB."
              id="branding-logo"
              label="Logo"
              maxBytes={MAX_LOGO_BYTES}
              onChange={(change) => {
                setImages((prev) => ({ ...prev, logo: change }))
              }}
              stored={stored.logo && { ...stored.logo, src: logo }}
              value={images.logo}
            />
            <ImageUploadField
              accept={BRANDING_IMAGE_TYPES}
              description="Shown in the dark theme. Without one the dark theme shows the logo."
              id="branding-logo-dark"
              label="Dark-theme logo"
              maxBytes={MAX_LOGO_BYTES}
              onChange={(change) => {
                setImages((prev) => ({ ...prev, logoDark: change }))
              }}
              stored={
                stored.logoDark && {
                  ...stored.logoDark,
                  src: storedAsset(stored, "logo-dark"),
                }
              }
              value={images.logoDark}
            />
            <ImageUploadField
              accept={BRANDING_IMAGE_TYPES}
              description="Shown in the browser tab. PNG, WebP or SVG, up to 32 KiB."
              id="branding-favicon"
              label="Favicon"
              maxBytes={MAX_FAVICON_BYTES}
              onChange={(favicon) => {
                setImages((prev) => ({ ...prev, favicon }))
              }}
              stored={
                stored.favicon && {
                  ...stored.favicon,
                  src: storedAsset(stored, "favicon"),
                }
              }
              value={images.favicon}
            />
          </div>
        </PanelSection>

        <PanelSection title="Preview">
          <BrandingPreview
            productName={values.productName?.trim() || DEFAULT_PRODUCT_NAME}
            themes={[
              { theme: "light", primary: previewColor(values.light), logo },
              {
                theme: "dark",
                primary: previewColor(values.dark),
                logo: logoDark,
              },
            ]}
          />
          <p className="mt-3 text-xs text-muted-foreground">
            The preview shows a removed image as gone at once; a newly chosen
            image shows after Save.
          </p>
        </PanelSection>

        {stored.updatedAt && (
          <p className="text-xs text-muted-foreground">
            Last changed by {stored.updatedBy ?? "an unknown principal"} at{" "}
            {new Date(stored.updatedAt).toLocaleString()}.
          </p>
        )}

        <div className="flex flex-wrap items-start justify-between gap-4">
          <FormActions
            error={save.error}
            errorMsg="Failed to save the branding"
            isPending={save.isPending}
            isSubmitting={isSubmitting}
            onCancel={() => {
              resetForm()
              setImages(KEEP_ALL)
            }}
            pendingLabel="Saving…"
            submitLabel="Save"
          />
          <Button
            disabled={stored.updatedAt === undefined}
            onClick={() => {
              setConfirmReset(true)
            }}
            type="button"
            variant="outline"
          >
            Reset to default
          </Button>
        </div>
      </FormLock>

      <ResetBrandingDialog
        onConfirm={() => {
          void onReset().catch(() => {
            // Shown under the form.
          })
        }}
        onOpenChange={setConfirmReset}
        open={confirmReset}
        pending={resetPending}
      />
    </form>
  )
}

function previewColor(value: string | undefined): string {
  const v = value?.trim() ?? ""

  return isHexColor(v) ? v : ""
}

const THEME_LABEL: Record<ThemeName, string> = {
  light: "Light theme",
  dark: "Dark theme",
}

// One theme's primary colour.
function ThemeColorField({
  control,
  theme,
}: {
  control: Control<BrandingFormData>
  theme: ThemeName
}) {
  return (
    <ColorField
      control={control}
      description="Buttons, links and the focus ring. Empty keeps the default colour."
      id={`branding-primary-${theme}`}
      label={`Primary colour, ${THEME_LABEL[theme].toLowerCase()}`}
      name={theme}
      placeholder={tokenHex(theme, "--primary")}
    />
  )
}
