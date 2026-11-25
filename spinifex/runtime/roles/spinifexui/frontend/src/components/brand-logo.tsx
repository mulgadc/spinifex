import { brandingAsset, getBranding, productName } from "@/lib/branding"
import { cn } from "@/lib/utils"

// The operator's logo, with its dark-theme variant when one is set. Without
// one the dark theme shows the same logo as is; the built-in mark is inverted.
export function BrandLogo({ className }: { className?: string }) {
  const branding = getBranding()
  const logo = brandingAsset(branding.logo)
  const alt = productName()

  if (!logo) {
    return (
      <img
        alt={alt}
        className={cn("dark:invert", className)}
        src="/mulga-logo.svg"
      />
    )
  }

  const logoDark = brandingAsset(branding.logoDark)

  if (!logoDark) {
    return (
      <img alt={alt} className={cn("object-contain", className)} src={logo} />
    )
  }

  return (
    <>
      <img
        alt={alt}
        className={cn("object-contain dark:hidden", className)}
        src={logo}
      />
      <img
        alt={alt}
        className={cn("hidden object-contain dark:block", className)}
        src={logoDark}
      />
    </>
  )
}
