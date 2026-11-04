import { type DataColumn, DataTable } from "@/components/data-table"
import { PanelSection } from "@/components/detail-panel"
import { Badge } from "@/components/ui/badge"
import { Progress } from "@/components/ui/progress"
import type { DiskFacts, FilesystemFacts, NodeFacts } from "@/queries/nodes"

const TIB = 1024 ** 4
const GIB = 1024 ** 3

// Drives and filesystems are sized in binary units, as lsblk and df -h are.
export function formatDiskSize(bytes: number): string {
  return bytes >= TIB
    ? `${(bytes / TIB).toFixed(2)} TiB`
    : `${(bytes / GIB).toFixed(1)} GiB`
}

const TRANSPORT_LABELS = new Map([
  ["nvme", "NVMe"],
  ["sata", "SATA"],
  ["usb", "USB"],
  ["virtio", "virtio"],
])

// Use% as df reports it: used over what an unprivileged user could hold,
// rounded up so a nearly full filesystem never reads as having room.
export function percentUsed(fs: FilesystemFacts): number | undefined {
  if (!fs.usage) {
    return undefined
  }

  const { used_bytes: used, avail_bytes: avail } = fs.usage

  return used + avail > 0 ? Math.ceil((used / (used + avail)) * 100) : undefined
}

const DRIVE_COLUMNS: DataColumn<DiskFacts>[] = [
  {
    id: "name",
    header: "Drive",
    sortValue: (disk) => disk.name,
    cell: (disk) => (
      <div className="flex flex-col">
        <span className="font-mono text-xs font-medium">{disk.name}</span>
        {disk.model && (
          <span
            className="max-w-40 truncate text-2xs text-muted-foreground"
            title={disk.model}
          >
            {disk.model}
          </span>
        )}
      </div>
    ),
  },
  {
    id: "size",
    header: "Size",
    sortValue: (disk) => disk.size_bytes,
    cell: (disk) => (
      <span className="text-xs tabular-nums">
        {formatDiskSize(disk.size_bytes)}
      </span>
    ),
  },
  {
    id: "transport",
    header: "Transport",
    sortValue: (disk) => disk.transport,
    cell: (disk) => (
      <span className="flex items-center gap-1.5 text-xs">
        {disk.transport &&
          (TRANSPORT_LABELS.get(disk.transport) ?? disk.transport)}
        <span className="text-muted-foreground">
          {disk.rotational ? "HDD" : "SSD"}
        </span>
        {disk.removable && <Badge variant="secondary">removable</Badge>}
      </span>
    ),
  },
]

const FILESYSTEM_COLUMNS: DataColumn<FilesystemFacts>[] = [
  {
    id: "mount",
    header: "Mount",
    sortValue: (fs) => fs.mount,
    cell: (fs) => (
      <div className="flex flex-col">
        <span className="font-mono text-xs font-medium">{fs.mount}</span>
        <span
          className="max-w-48 truncate font-mono text-2xs text-muted-foreground"
          title={fs.bind_of ? `bind of ${fs.bind_of}` : fs.source}
        >
          {fs.bind_of ? `bind of ${fs.bind_of}` : fs.source}
        </span>
      </div>
    ),
  },
  {
    id: "disk",
    header: "Drive",
    hideBelow: "md",
    sortValue: (fs) => fs.disk,
    cell: (fs) => (
      <span className="font-mono text-xs">
        {fs.disk || "-"}{" "}
        <span className="text-muted-foreground">{fs.type}</span>
      </span>
    ),
  },
  {
    id: "use",
    header: "Use%",
    sortValue: (fs) => percentUsed(fs),
    cell: (fs) => <Usage fs={fs} />,
  },
]

function Usage({ fs }: { fs: FilesystemFacts }) {
  const percent = percentUsed(fs)

  return (
    <div className="flex min-w-24 flex-col gap-1">
      {percent !== undefined && (
        <div className="flex items-center gap-2">
          <Progress
            aria-label={`${fs.mount} used`}
            className="w-16"
            max={100}
            value={percent}
          />
          <span className="text-xs tabular-nums">{percent}%</span>
        </div>
      )}
      {fs.usage && (
        <span className="text-2xs text-muted-foreground tabular-nums">
          {formatDiskSize(fs.usage.used_bytes)} /{" "}
          {formatDiskSize(fs.usage.size_bytes)}
        </span>
      )}
      {fs.error && <span className="text-2xs text-warning">{fs.error}</span>}
    </div>
  )
}

const SPINIFEX_DATA = "/var/lib/spinifex"

function underSpinifexData(fs: FilesystemFacts): boolean {
  return fs.mount === SPINIFEX_DATA || fs.mount.startsWith(`${SPINIFEX_DATA}/`)
}

interface MountGroups {
  spinifex: FilesystemFacts[]
  system: FilesystemFacts[]
}

// Spinifex's own mounts: its data directory and what is mounted under it, and
// any other path that binds the same directory, as shared propagation does.
export function splitSpinifexMounts(
  filesystems: FilesystemFacts[],
): MountGroups {
  const dataBinds = new Set(
    filesystems.filter(underSpinifexData).flatMap((fs) => fs.bind_of ?? []),
  )

  const isSpinifex = (fs: FilesystemFacts) =>
    underSpinifexData(fs) ||
    (fs.bind_of !== undefined && dataBinds.has(fs.bind_of))

  return {
    spinifex: filesystems.filter(isSpinifex),
    system: filesystems.filter((fs) => !isSpinifex(fs)),
  }
}

function MountTable({
  caption,
  rows,
}: {
  caption: string
  rows: FilesystemFacts[]
}) {
  return (
    <DataTable
      caption={caption}
      columns={FILESYSTEM_COLUMNS}
      empty="No filesystems reported."
      getRowId={(fs) => `${fs.mount} ${fs.source} ${fs.bind_of ?? ""}`}
      rows={rows}
    />
  )
}

// The node's drives as lsblk -d lists them, then its mounted filesystems as
// df does, Spinifex's apart from the system's. Pseudo filesystems such as
// tmpfs and efivarfs are not reported.
export function NodeDisks({ facts }: { facts: NodeFacts | undefined }) {
  if (!facts) {
    return (
      <PanelSection title="Disks">
        <p className="text-xs text-muted-foreground">
          This node did not report its disks.
        </p>
      </PanelSection>
    )
  }

  const { spinifex, system } = splitSpinifexMounts(facts.filesystems ?? [])

  return (
    <>
      <PanelSection title="Drives">
        <DataTable
          caption="Physical drives"
          columns={DRIVE_COLUMNS}
          empty="No drives reported."
          getRowId={(disk) => disk.name}
          rows={facts.disks ?? []}
        />
      </PanelSection>
      {spinifex.length > 0 && (
        <PanelSection title="Spinifex mounts">
          <MountTable caption="Spinifex mounts" rows={spinifex} />
        </PanelSection>
      )}
      <PanelSection title="System mounts">
        <MountTable caption="System mounts" rows={system} />
      </PanelSection>
    </>
  )
}
