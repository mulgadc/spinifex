import { z } from "zod"

import { isHexColor } from "@/lib/theme-tokens"

// Wire types of the /admin console branding methods, and the Settings form.

export interface BrandingColors {
  light?: string
  dark?: string
}

export interface BrandingImageInfo {
  contentType: string
  size: number
}

export interface BrandingImage {
  contentType: string
  // Base64.
  data: string
}

export interface ConsoleBrandingResponse {
  revision: number
  productName?: string
  // Absent keeps the built-in link; empty hides it.
  docsUrl?: string
  primary?: BrandingColors
  logo?: BrandingImageInfo
  logoDark?: BrandingImageInfo
  favicon?: BrandingImageInfo
  updatedBy?: string
  updatedAt?: string
}

// An image field is left out to keep the stored image, null to remove it, and
// set to replace it.
export interface PutConsoleBrandingRequest {
  productName?: string
  docsUrl?: string
  primary?: BrandingColors
  logo?: BrandingImage | null
  logoDark?: BrandingImage | null
  favicon?: BrandingImage | null
}

export const BRANDING_IMAGE_TYPES = [
  "image/png",
  "image/webp",
  "image/svg+xml",
] as const

export const MAX_LOGO_BYTES = 128 * 1024
export const MAX_FAVICON_BYTES = 32 * 1024

export const MAX_PRODUCT_NAME_LENGTH = 40

export type DocsLink = "default" | "custom" | "hidden"

// Only the shape is checked; the gateway writes the colour into a stylesheet.
const colorField = z
  .string()
  .trim()
  .refine((value) => value === "" || isHexColor(value), {
    message: "Enter a colour as #RRGGBB.",
  })

export const brandingFormSchema = z
  .object({
    productName: z
      .string()
      .trim()
      .max(
        MAX_PRODUCT_NAME_LENGTH,
        `At most ${MAX_PRODUCT_NAME_LENGTH} characters.`,
      ),
    docs: z.enum(["default", "custom", "hidden"]),
    docsUrl: z.string().trim(),
    light: colorField,
    dark: colorField,
  })
  .superRefine((form, ctx) => {
    if (form.docs !== "custom") {
      return
    }

    let ok = false

    try {
      ok = new URL(form.docsUrl).protocol === "https:"
    } catch {
      ok = false
    }

    if (!ok) {
      ctx.addIssue({
        code: "custom",
        message: "Enter an https:// URL.",
        path: ["docsUrl"],
      })
    }
  })

export type BrandingFormData = z.infer<typeof brandingFormSchema>

export function brandingFormValues(
  stored: ConsoleBrandingResponse | undefined,
): BrandingFormData {
  let docs: DocsLink = "default"

  if (stored?.docsUrl === "") {
    docs = "hidden"
  } else if (stored?.docsUrl !== undefined) {
    docs = "custom"
  }

  return {
    productName: stored?.productName ?? "",
    docs,
    docsUrl: docs === "custom" ? (stored?.docsUrl ?? "") : "",
    light: stored?.primary?.light ?? "",
    dark: stored?.primary?.dark ?? "",
  }
}

// What a save does with one stored image.
export type ImageChange =
  | { kind: "keep" }
  | { kind: "remove" }
  | { kind: "set"; image: BrandingImage & { name: string; size: number } }

export interface BrandingImageChanges {
  logo: ImageChange
  logoDark: ImageChange
  favicon: ImageChange
}

function imageField(change: ImageChange): BrandingImage | null | undefined {
  if (change.kind === "set") {
    return { contentType: change.image.contentType, data: change.image.data }
  }

  return change.kind === "remove" ? null : undefined
}

// The whole record for one PutConsoleBranding: a field left empty takes the
// default, and an image nobody changed is left out so it is not resent.
export function brandingRequest(
  form: BrandingFormData,
  images: BrandingImageChanges,
): PutConsoleBrandingRequest {
  const docsUrl: Record<DocsLink, string | undefined> = {
    default: undefined,
    custom: form.docsUrl,
    hidden: "",
  }

  return {
    productName: form.productName || undefined,
    docsUrl: docsUrl[form.docs],
    primary: {
      light: form.light || undefined,
      dark: form.dark || undefined,
    },
    logo: imageField(images.logo),
    logoDark: imageField(images.logoDark),
    favicon: imageField(images.favicon),
  }
}

// A reset removes every value, the images too, in one revision.
export function resetBrandingRequest(): PutConsoleBrandingRequest {
  return { logo: null, logoDark: null, favicon: null }
}
