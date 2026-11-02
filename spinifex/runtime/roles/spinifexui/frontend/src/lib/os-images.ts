import type { Image, InstanceTypeInfo } from "@aws-sdk/client-ec2"

export type Architecture = "x86_64" | "arm64"

// Catalog names read <distro>-<version>-<arch>, sometimes behind an "ami-"
// prefix: ami-ubuntu-26.04-x86_64, oracle-10.1-arm64, rocky-10-x86_64.
const IMAGE_NAME_RE =
  /^(?:ami-)?(?<distro>[a-z][a-z0-9]*(?:-[a-z][a-z0-9]*)*)-(?<version>\d+(?:\.\d+)*)-(?<arch>x86_64|amd64|arm64|aarch64)$/iu

export interface ParsedImageName {
  distro: string
  version: string
  arch: Architecture
}

const ARCHITECTURE_ALIASES = new Map<string, Architecture>([
  ["x86_64", "x86_64"],
  ["amd64", "x86_64"],
  ["arm64", "arm64"],
  ["aarch64", "arm64"],
])

export function normaliseArchitecture(
  value: string | undefined,
): Architecture | undefined {
  return value ? ARCHITECTURE_ALIASES.get(value.toLowerCase()) : undefined
}

export function parseImageName(
  name: string | undefined,
): ParsedImageName | undefined {
  const groups = IMAGE_NAME_RE.exec(name ?? "")?.groups

  if (!groups?.distro || !groups.version || !groups.arch) {
    return undefined
  }

  const arch = normaliseArchitecture(groups.arch)

  if (!arch) {
    return undefined
  }

  return { distro: groups.distro.toLowerCase(), version: groups.version, arch }
}

// Numeric per segment, so 10.1 sorts above 9.8 and 24.10 above 24.4; a
// missing segment counts as zero, so 10 and 10.0 are equal.
export function compareVersions(a: string, b: string): number {
  const left = a.split(".").map(Number)
  const right = b.split(".").map(Number)

  for (let i = 0; i < Math.max(left.length, right.length); i++) {
    const diff = (left[i] ?? 0) - (right[i] ?? 0)

    if (diff !== 0) {
      return diff
    }
  }

  return 0
}

// The image's own Architecture is what RunInstances launches against, so it
// wins over the name when both are present.
export function imageArchitecture(image: Image): Architecture | undefined {
  return (
    normaliseArchitecture(image.Architecture) ??
    parseImageName(image.Name)?.arch
  )
}

export interface DistroImage {
  image: Image
  version: string
}

export interface DistroGroup {
  distro: string
  // Highest version first.
  versions: DistroImage[]
}

export interface ImageCatalog {
  distros: DistroGroup[]
  // Images whose names do not parse, kept reachable outside the logo tiles.
  other: Image[]
}

export function buildImageCatalog(
  images: readonly Image[],
  arch: Architecture | undefined,
): ImageCatalog {
  const byDistro = new Map<string, DistroImage[]>()
  const other: Image[] = []

  for (const image of images) {
    const imageArch = imageArchitecture(image)

    // An image that reports no architecture stays offered under every one.
    if (arch && imageArch && imageArch !== arch) {
      continue
    }

    const parsed = parseImageName(image.Name)

    if (!parsed) {
      other.push(image)
      continue
    }

    const list = byDistro.get(parsed.distro) ?? []
    list.push({ image, version: parsed.version })
    byDistro.set(parsed.distro, list)
  }

  const distros = [...byDistro.entries()]
    .map(([distro, versions]) => ({
      distro,
      versions: versions.toSorted((a, b) =>
        compareVersions(b.version, a.version),
      ),
    }))
    .toSorted((a, b) => a.distro.localeCompare(b.distro))

  return { distros, other }
}

export function imageArchitectures(images: readonly Image[]): Architecture[] {
  const found = new Set<Architecture>()

  for (const image of images) {
    const arch = imageArchitecture(image)

    if (arch) {
      found.add(arch)
    }
  }

  return (["x86_64", "arm64"] as const).filter((arch) => found.has(arch))
}

// A type that reports no architecture is offered for every image rather than
// hidden, since the backend would decide.
export function instanceTypeSupports(
  type: InstanceTypeInfo,
  arch: Architecture | undefined,
): boolean {
  const supported = type.ProcessorInfo?.SupportedArchitectures ?? []

  if (!arch || supported.length === 0) {
    return true
  }

  return supported.some((value) => normaliseArchitecture(value) === arch)
}

const DISTRO_LABELS = new Map<string, string>([
  ["almalinux", "AlmaLinux"],
  ["alpine", "Alpine"],
  ["amazon", "Amazon Linux"],
  ["centos", "CentOS"],
  ["debian", "Debian"],
  ["fedora", "Fedora"],
  ["freebsd", "FreeBSD"],
  ["opensuse", "openSUSE"],
  ["oracle", "Oracle Linux"],
  ["rhel", "Red Hat"],
  ["rocky", "Rocky Linux"],
  ["ubuntu", "Ubuntu"],
])

// A distro the table does not know is title-cased from its catalog name.
export function distroLabel(distro: string): string {
  return (
    DISTRO_LABELS.get(distro) ??
    distro
      .split("-")
      .map((word) => word.charAt(0).toUpperCase() + word.slice(1))
      .join(" ")
  )
}

// The image a fresh form starts on: the newest of the preferred distro if the
// catalog has it, else the newest of the first distro, else any other image.
export function preferredImage(
  catalog: ImageCatalog,
  distro = "ubuntu",
): Image | undefined {
  const group =
    catalog.distros.find((g) => g.distro === distro) ?? catalog.distros[0]

  return group?.versions[0]?.image ?? catalog.other[0]
}

// The latest image of a distro in the catalog, for carrying a distro choice
// across an architecture change.
export function latestOfDistro(
  catalog: ImageCatalog,
  distro: string | undefined,
): Image | undefined {
  return catalog.distros.find((g) => g.distro === distro)?.versions[0]?.image
}
