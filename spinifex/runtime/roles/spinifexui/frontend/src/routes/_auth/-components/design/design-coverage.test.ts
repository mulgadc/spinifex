import { describe, expect, it } from "vitest"

const COMPONENTS = import.meta.glob<string>(
  ["/src/components/*.tsx", "!/src/components/*.test.tsx"],
  { query: "?raw", import: "default", eager: true },
)

const CATALOGUE = import.meta.glob<string>("./*.tsx", {
  query: "?raw",
  import: "default",
  eager: true,
})

// Shared components the catalogue does not show, each for a stated reason. A
// new shared component goes on the page, not on this list.
const NOT_SHOWN = {
  "admin-dashboard-panel": "reads the cluster's live node state",
  "app-breadcrumbs": "part of the shell the catalogue renders inside",
  "bulk-delete": "drives real delete mutations",
  "command-palette": "part of the shell; opened with the shortcut",
  "editor-shell": "the policy editor's frame, shown with its editor",
  "gpu-instance-type-select": "reads the cluster's GPU inventory",
  "gpu-inventory-card": "reads the cluster's GPU inventory",
  header: "part of the shell the catalogue renders inside",
  "nodes-table": "reads the cluster's live node state",
  "not-found": "the router's own page",
  "resource-delete-dialog": "drives a real delete mutation",
  "resource-view": "the list and panel layout every list page uses",
  "sandbox-welcome-dialog": "shown once to a new sandbox account",
  "security-group-picker": "a SwitchList over live security groups",
  "system-image-required": "rechecks the account's images",
  "tags-editor": "drives a real tagging mutation",
  "theme-provider": "part of the shell",
  "theme-switch": "part of the shell, in the header",
  "upload-progress-dialog": "tracks real S3 uploads",
  "vpc-select-field": "reads the account's VPCs",
} satisfies Record<string, string>

function catalogueSource(): string {
  return Object.values(CATALOGUE).join("\n")
}

function sharedComponents(): string[] {
  return Object.keys(COMPONENTS).map((path) =>
    path.replace(/^\/src\/components\//u, "").replace(/\.tsx$/u, ""),
  )
}

describe("design catalogue coverage", () => {
  it("shows every shared component, or says why not", () => {
    const source = catalogueSource()

    const missing = sharedComponents().filter(
      (name) =>
        !(name in NOT_SHOWN) && !source.includes(`"@/components/${name}"`),
    )

    expect(missing).toStrictEqual([])
  })

  it("lists only components that exist and are not shown", () => {
    const source = catalogueSource()
    const existing = new Set(sharedComponents())

    const stale = Object.keys(NOT_SHOWN).filter(
      (name) =>
        !existing.has(name) || source.includes(`"@/components/${name}"`),
    )

    expect(stale).toStrictEqual([])
  })
})
