import { brandingAsset, getBranding, productName } from "@/lib/branding"
import { cn } from "@/lib/utils"

const BUILT_IN = "/mulga-logo.svg"

// Each theme's logo on its own: the dark theme falls back to the logo, and a
// theme with no logo shows the built-in mark, inverted in the dark theme.
export function BrandLogo({ className }: { className?: string }) {
  const branding = getBranding()
  const alt = productName()
  const light = brandingAsset(branding.logo) ?? BUILT_IN
  const dark = brandingAsset(branding.logoDark) ?? brandingAsset(branding.logo)

  if (light === BUILT_IN && !dark) {
    return (
      <img alt={alt} className={cn("dark:invert", className)} src={light} />
    )
  }

  if (light === dark || !dark) {
    return (
      <img alt={alt} className={cn("object-contain", className)} src={light} />
    )
  }

  return (
    <>
      <img
        alt={alt}
        className={cn("object-contain dark:hidden", className)}
        src={light}
      />
      <img
        alt={alt}
        className={cn("hidden object-contain dark:block", className)}
        src={dark}
      />
    </>
  )
}
