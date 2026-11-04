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
          <span className="max-w-56 truncate text-2xs text-muted-foreground">
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
      <span className="text-xs">
        {disk.transport
          ? (TRANSPORT_LABELS.get(disk.transport) ?? disk.transport)
          : "-"}
      </span>
    ),
  },
  {
    id: "media",
    header: "Media",
    sortValue: (disk) => (disk.rotational ? "HDD" : "SSD"),
    cell: (disk) => (
      <span className="flex items-center gap-1.5 text-xs">
        {disk.rotational ? "HDD" : "SSD"}
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
        <span className="max-w-64 truncate font-mono text-2xs text-muted-foreground">
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
    id: "size",
    header: "Size",
    sortValue: (fs) => fs.usage?.size_bytes,
    cell: (fs) => <Bytes value={fs.usage?.size_bytes} />,
  },
  {
    id: "used",
    header: "Used",
    hideBelow: "lg",
    sortValue: (fs) => fs.usage?.used_bytes,
    cell: (fs) => <Bytes value={fs.usage?.used_bytes} />,
  },
  {
    id: "avail",
    header: "Available",
    sortValue: (fs) => fs.usage?.avail_bytes,
    cell: (fs) => <Bytes value={fs.usage?.avail_bytes} />,
  },
  {
    id: "use",
    header: "Use%",
    sortValue: (fs) => percentUsed(fs),
    cell: (fs) => <Usage fs={fs} />,
  },
]

function Bytes({ value }: { value: number | undefined }) {
  return (
    <span className="text-xs tabular-nums">
      {value === undefined ? "-" : formatDiskSize(value)}
    </span>
  )
}

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
      {fs.error && <span className="text-2xs text-warning">{fs.error}</span>}
    </div>
  )
}

// The node's drives as lsblk -d lists them, then its mounted filesystems as
// df does. Pseudo filesystems such as tmpfs and efivarfs are not reported.
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
      <PanelSection title="Filesystems">
        <DataTable
          caption="Mounted filesystems"
          columns={FILESYSTEM_COLUMNS}
          empty="No filesystems reported."
          getRowId={(fs) => fs.mount}
          rows={facts.filesystems ?? []}
        />
      </PanelSection>
    </>
  )
}
