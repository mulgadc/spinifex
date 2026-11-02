import { Disc3, Server } from "lucide-react"

// `no-inline` keeps each mark a file served from the console's own origin:
// an inlined data: URI would break the CSP's `img-src 'self'`.
import alpine from "@/assets/os/alpine.svg?no-inline"
import centos from "@/assets/os/centos.svg?no-inline"
import debian from "@/assets/os/debian.svg?no-inline"
import rocky from "@/assets/os/rocky.svg?no-inline"
import ubuntu from "@/assets/os/ubuntu.svg?no-inline"

const MARKS = new Map<string, string>([
  ["alpine", alpine],
  ["centos", centos],
  ["debian", debian],
  ["rocky", rocky],
  ["ubuntu", ubuntu],
])

interface OsMarkProps {
  // Absent for the "Other images" tile.
  distro?: string
  size?: "sm" | "md"
}

// The distribution's own mark where we have one, and a generic server for
// any other distribution the catalog parses.
export function OsMark({ distro, size = "md" }: OsMarkProps) {
  const className = size === "md" ? "size-8" : "size-5"

  if (!distro) {
    return (
      <Disc3 aria-hidden className={`${className} text-muted-foreground`} />
    )
  }

  const mark = MARKS.get(distro)

  if (!mark) {
    return (
      <Server
        aria-hidden
        className={`${className} text-muted-foreground`}
        data-distro={distro}
      />
    )
  }

  return (
    <img
      alt=""
      className={`${className} object-contain`}
      data-distro={distro}
      src={mark}
    />
  )
}
