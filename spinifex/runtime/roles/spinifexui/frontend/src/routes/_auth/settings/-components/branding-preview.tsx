import type { CSSProperties } from "react"

import { Button } from "@/components/ui/button"
import { type ThemeName, tokenHex } from "@/lib/brand-contrast"
import { cn } from "@/lib/utils"

type CSSVariables = CSSProperties & Record<`--${string}`, string>

export interface PreviewTheme {
  theme: ThemeName
  // The colour to show, or empty for the default.
  primary: string
  // The stored logo for this theme; without one the built-in mark is shown.
  logo?: string
}

// The theme's own tokens go on the box, so each theme shows as it will
// whichever theme the page itself is in.
function PreviewCard({
  productName,
  theme,
  primary,
  logo,
}: PreviewTheme & { productName: string }) {
  const colour = primary === "" ? tokenHex(theme, "--primary") : primary

  const tokens: CSSVariables = {
    "--background": tokenHex(theme, "--background"),
    "--card": tokenHex(theme, "--card"),
    "--card-foreground": tokenHex(theme, "--foreground"),
    "--foreground": tokenHex(theme, "--foreground"),
    "--primary": colour,
    "--primary-foreground": tokenHex(theme, "--primary-foreground"),
    "--ring": colour,
  }

  return (
    <figure
      className="space-y-3 rounded-lg border bg-background p-4 text-foreground"
      style={tokens}
    >
      <figcaption className="text-xs opacity-70">
        {theme === "light" ? "Light theme" : "Dark theme"}
      </figcaption>
      <div className="flex items-center gap-2">
        <img
          alt=""
          className={cn(
            "size-6 object-contain",
            !logo && theme === "dark" && "invert",
          )}
          src={logo ?? "/mulga-logo.svg"}
        />
        <span className="font-semibold">{productName}</span>
      </div>
      <div className="space-y-3 rounded-md bg-card p-3 text-card-foreground">
        <p className="text-sm">Sign in with {productName} credentials</p>
        <div className="flex flex-wrap items-center gap-3">
          <Button tabIndex={-1} type="button">
            Sign in
          </Button>
          <Button tabIndex={-1} type="button" variant="link">
            Documentation
          </Button>
        </div>
      </div>
    </figure>
  )
}

export function BrandingPreview({
  productName,
  themes,
}: {
  productName: string
  themes: readonly PreviewTheme[]
}) {
  return (
    <div className="grid gap-4 md:grid-cols-2">
      {themes.map((theme) => (
        <PreviewCard key={theme.theme} productName={productName} {...theme} />
      ))}
    </div>
  )
}
