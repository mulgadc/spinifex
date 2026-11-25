import { z } from "zod"

// The operator's branding, served by the gateway through /branding/. A failed
// or slow load leaves the built-in brand, so it never stops the console.

export const BRANDING_BUDGET_MS = 2000

export const DEFAULT_PRODUCT_NAME = "Spinifex"
export const DEFAULT_DOCS_URL = "https://docs.mulgadc.com"

// Asset paths are relative names with a revision; anything else is ignored.
const assetPath = z
  .string()
  .regex(/^[a-z][a-z.-]*\?rev=\d+$/)
  .optional()
  .catch(undefined)

const brandingSchema = z.object({
  enabled: z.boolean().catch(false),
  revision: z.number().optional().catch(undefined),
  productName: z.string().max(40).optional().catch(undefined),
  docsUrl: z.string().optional().catch(undefined),
  stylesheet: assetPath,
  logo: assetPath,
  logoDark: assetPath,
  favicon: assetPath,
})

export type Branding = z.infer<typeof brandingSchema>

const builtIn: Branding = { enabled: false }

let branding: Branding = builtIn

export function getBranding(): Branding {
  return branding
}

// The name shown in the sidebar and on the login page.
export function productName(): string {
  return (branding.enabled && branding.productName) || DEFAULT_PRODUCT_NAME
}

// The Docs link, or null when the operator has hidden it. Only an https URL is
// used, so a bad value cannot become a javascript: link.
export function docsUrl(): string | null {
  if (!branding.enabled || branding.docsUrl === undefined) {
    return DEFAULT_DOCS_URL
  }

  if (branding.docsUrl === "") {
    return null
  }

  try {
    return new URL(branding.docsUrl).protocol === "https:"
      ? branding.docsUrl
      : DEFAULT_DOCS_URL
  } catch {
    return DEFAULT_DOCS_URL
  }
}

export function brandingAsset(path: string | undefined): string | undefined {
  return path === undefined ? undefined : `/branding/${path}`
}

// Waits for the stylesheet, but never past the signal: when the budget ends the
// console renders in the default colours and the sheet applies when it lands.
async function waitForStylesheet(
  link: HTMLLinkElement,
  signal: AbortSignal,
): Promise<void> {
  if (signal.aborted) {
    return
  }

  // oxlint-disable-next-line promise/avoid-new -- the only way to await the first of three DOM events
  await new Promise<void>((resolve) => {
    const done = () => {
      resolve()
    }

    link.addEventListener("load", done, { once: true })
    link.addEventListener("error", done, { once: true })
    signal.addEventListener("abort", done, { once: true })
  })
}

// Loads the branding under one budget covering the JSON and the stylesheet,
// then applies the stylesheet and favicon. It never throws.
export async function loadBranding(
  budgetMs: number = BRANDING_BUDGET_MS,
): Promise<void> {
  const signal = AbortSignal.timeout(budgetMs)

  try {
    const response = await fetch("/branding/branding.json", {
      cache: "no-store",
      signal,
    })

    if (!response.ok) {
      return
    }

    const parsed = brandingSchema.safeParse(await response.json())

    if (!parsed.success || !parsed.data.enabled) {
      return
    }

    branding = parsed.data
  } catch {
    return
  }

  const favicon = brandingAsset(branding.favicon)

  if (favicon) {
    for (const icon of document.querySelectorAll('link[rel="icon"]')) {
      icon.remove()
    }

    const icon = document.createElement("link")
    icon.rel = "icon"
    icon.href = favicon
    document.head.append(icon)
  }

  const stylesheet = brandingAsset(branding.stylesheet)

  if (stylesheet) {
    const link = document.createElement("link")
    link.rel = "stylesheet"
    link.href = stylesheet
    document.head.append(link)
    await waitForStylesheet(link, signal)
  }
}

// Test hook: return to the built-in brand.
export function resetBrandingForTest(): void {
  branding = builtIn
}
