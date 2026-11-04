import { render, screen, within } from "@testing-library/react"
import { describe, expect, it } from "vitest"

import type { NodeFacts } from "@/queries/nodes"

import {
  formatDiskSize,
  NodeDisks,
  percentUsed,
  splitSpinifexMounts,
} from "./node-disks"

const GIB = 1024 ** 3

// SAFETY: NodeDisks reads only disks and filesystems, so hardware fields are left out.
const node = {
  node: "bottlebrush",
  disks: [
    {
      name: "nvme0n1",
      path: "/dev/nvme0n1",
      size_bytes: 2_000_398_934_016,
      model: "Samsung SSD 990 PRO 2TB",
      transport: "nvme",
    },
    {
      name: "sda",
      path: "/dev/sda",
      size_bytes: 8_001_563_222_016,
      model: "ST8000NM000A-2KE",
      transport: "sata",
      rotational: true,
    },
    { name: "sdc", path: "/dev/sdc", size_bytes: 959_656_755_200 },
  ],
  filesystems: [
    {
      mount: "/",
      source: "/dev/nvme0n1p2",
      disk: "nvme0n1",
      type: "ext4",
      usage: {
        size_bytes: 100 * GIB,
        used_bytes: 60 * GIB,
        avail_bytes: 35 * GIB,
      },
    },
    {
      mount: "/var/lib/spinifex",
      source: "/dev/sda1",
      disk: "sda",
      type: "xfs",
      bind_of: "/mnt/disk2/spinifex",
      usage: {
        size_bytes: 7000 * GIB,
        used_bytes: 1000 * GIB,
        avail_bytes: 6000 * GIB,
      },
    },
    {
      mount: "/srv",
      source: "/dev/sda2",
      disk: "sda",
      type: "ext4",
      usage: { size_bytes: 50 * GIB, used_bytes: 47 * GIB, avail_bytes: 0 },
    },
    {
      mount: "/mnt/nfs",
      source: "/dev/sdc1",
      disk: "sdc",
      type: "ext4",
      error: "not answering",
    },
  ],
} as NodeFacts

function rowOf(table: string, name: string): HTMLElement {
  const row = within(screen.getByRole("table", { name: table }))
    .getByText(name)
    .closest("tr")

  if (!row) {
    throw new Error(`no row for ${name}`)
  }

  return row
}

function headersOf(table: string): (string | null)[] {
  return within(screen.getByRole("table", { name: table }))
    .getAllByRole("columnheader")
    .map((th) => th.textContent)
}

describe("NodeDisks", () => {
  it("lists drives with size, transport and media", () => {
    render(<NodeDisks facts={node} />)

    const nvme = rowOf("Physical drives", "nvme0n1")

    expect(nvme).toHaveTextContent("Samsung SSD 990 PRO 2TB")
    expect(nvme).toHaveTextContent("1.82 TiB")
    expect(nvme).toHaveTextContent("NVMe")
    expect(nvme).toHaveTextContent("SSD")
    expect(rowOf("Physical drives", "sda")).toHaveTextContent("SATAHDD")
    expect(rowOf("Physical drives", "sdc")).toHaveTextContent("893.8 GiBSSD")
  })

  it("fits each table in the panel with no media or size columns", () => {
    render(<NodeDisks facts={node} />)

    expect(headersOf("Physical drives")).toStrictEqual([
      "Drive",
      "Size",
      "Transport",
    ])
    expect(headersOf("System mounts")).toStrictEqual(["Mount", "Drive", "Use%"])
  })

  it("lists filesystems with df's figures and names a bind by what it shows", () => {
    render(<NodeDisks facts={node} />)

    const root = rowOf("System mounts", "/")

    expect(root).toHaveTextContent("64%")
    expect(root).toHaveTextContent("60.0 GiB / 100.0 GiB")
    expect(rowOf("Spinifex mounts", "/var/lib/spinifex")).toHaveTextContent(
      "bind of /mnt/disk2/spinifex",
    )
  })

  it("shows a full filesystem as full rather than blank", () => {
    render(<NodeDisks facts={node} />)

    const full = rowOf("System mounts", "/srv")

    expect(full).toHaveTextContent("100%")
    expect(full).toHaveTextContent("47.0 GiB / 50.0 GiB")
  })

  it("groups Spinifex's mounts apart from the system's", () => {
    expect(
      splitSpinifexMounts([
        { mount: "/", source: "/dev/nvme0n1p2", type: "ext4" },
        { mount: "/mnt/disk3", source: "/dev/sdb1", type: "ext4" },
        {
          mount: "/var/lib/spinifex/predastore",
          source: "/dev/sdb1",
          type: "ext4",
          bind_of: "/mnt/disk3/predastore",
        },
        {
          mount: "/mnt/disk2/spinifex/predastore",
          source: "/dev/sdb1",
          type: "ext4",
          bind_of: "/mnt/disk3/predastore",
        },
        { mount: "/var/lib/spinifex-old", source: "/dev/sdc1", type: "ext4" },
      ]),
    ).toStrictEqual({
      spinifex: [
        expect.objectContaining({ mount: "/var/lib/spinifex/predastore" }),
        expect.objectContaining({ mount: "/mnt/disk2/spinifex/predastore" }),
      ],
      system: [
        expect.objectContaining({ mount: "/" }),
        expect.objectContaining({ mount: "/mnt/disk3" }),
        expect.objectContaining({ mount: "/var/lib/spinifex-old" }),
      ],
    })
  })

  it("leaves out the Spinifex section on a node with no Spinifex mounts", () => {
    render(
      <NodeDisks
        facts={{ ...node, filesystems: node.filesystems?.slice(0, 1) }}
      />,
    )

    expect(
      screen.queryByRole("table", { name: "Spinifex mounts" }),
    ).not.toBeInTheDocument()
  })

  it("says why a filesystem has no sizes", () => {
    render(<NodeDisks facts={node} />)

    const hung = rowOf("System mounts", "/mnt/nfs")

    expect(hung).toHaveTextContent("not answering")
    expect(hung).not.toHaveTextContent("GiB /")
    expect(within(hung).queryByRole("progressbar")).not.toBeInTheDocument()
  })

  it("says when the node reported nothing", () => {
    render(<NodeDisks facts={undefined} />)

    expect(
      screen.getByText("This node did not report its disks."),
    ).toBeInTheDocument()
  })
})

describe("disk figures", () => {
  it("switches to TiB at one TiB", () => {
    expect(formatDiskSize(1023 * GIB)).toBe("1023.0 GiB")
    expect(formatDiskSize(1024 * GIB)).toBe("1.00 TiB")
  })

  it("rounds use up, as df does", () => {
    expect(
      percentUsed({
        mount: "/",
        source: "",
        type: "ext4",
        usage: { size_bytes: 1000, used_bytes: 901, avail_bytes: 99 },
      }),
    ).toBe(91)
  })
})
