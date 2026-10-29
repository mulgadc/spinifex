import { Disc3 } from "lucide-react"

// oxlint-disable shadcn/no-raw-colors -- a distro mark wears its own colour, not a theme token

const DISTROS = new Map<string, { colour: string; monogram: string }>([
  ["almalinux", { colour: "#0F4266", monogram: "Al" }],
  ["alpine", { colour: "#0D597F", monogram: "A" }],
  ["amazon", { colour: "#232F3E", monogram: "AL" }],
  ["centos", { colour: "#932279", monogram: "C" }],
  ["debian", { colour: "#A80030", monogram: "D" }],
  ["fedora", { colour: "#294172", monogram: "F" }],
  ["freebsd", { colour: "#AB2B28", monogram: "Fb" }],
  ["opensuse", { colour: "#73BA25", monogram: "S" }],
  ["oracle", { colour: "#C74634", monogram: "O" }],
  ["rhel", { colour: "#CC0000", monogram: "RH" }],
  ["rocky", { colour: "#10B981", monogram: "R" }],
  ["ubuntu", { colour: "#E95420", monogram: "U" }],
])

const FALLBACK_COLOUR = "#52525B"

interface OsMarkProps {
  // Absent for the "Other images" tile.
  distro?: string
  size?: "sm" | "md"
}

// A coloured monogram for a distribution. Drawn, not a vendor logo, so it
// carries no trademark and loads under the console's self-only CSP.
export function OsMark({ distro, size = "md" }: OsMarkProps) {
  const className = size === "md" ? "size-8" : "size-5"

  if (!distro) {
    return (
      <Disc3 aria-hidden className={`${className} text-muted-foreground`} />
    )
  }

  const style = DISTROS.get(distro)
  const monogram = style?.monogram ?? distro.charAt(0).toUpperCase()

  return (
    <svg
      aria-hidden
      className={className}
      data-distro={distro}
      viewBox="0 0 32 32"
    >
      <rect
        fill={style?.colour ?? FALLBACK_COLOUR}
        height={32}
        rx={8}
        width={32}
      />
      <text
        dominantBaseline="central"
        fill="#fff"
        fontFamily="inherit"
        fontSize={monogram.length > 1 ? 13 : 17}
        fontWeight={700}
        textAnchor="middle"
        x={16}
        y={16.5}
      >
        {monogram}
      </text>
    </svg>
  )
}
