import { describe, expect, it } from "vitest"

// Raw source of every route file, keyed by its path from the frontend root.
const sources = Object.fromEntries(
  Object.entries(
    import.meta.glob<string>(["/src/routes/**/*.tsx", "!**/*.test.tsx"], {
      eager: true,
      import: "default",
      query: "?raw",
    }),
  ).map(([path, source]) => [path.slice(1), source]),
)

// Route files that still hand-write a <table>. Pages move to the shared
// DataTable instead, so this list may only shrink: drop a file from it in the
// change that removes its table.
const HAND_WRITTEN_TABLES = new Set([
  "src/routes/_auth/bedrock/(catalog)/-components/model-catalog-page.tsx",
  "src/routes/_auth/bedrock/(model-access)/-components/model-access-page.tsx",
  "src/routes/_auth/ec2/(launch-templates)/describe-launch-templates/$id.tsx",
  "src/routes/_auth/ec2/(load-balancers)/-components/describe-load-balancers-page.tsx",
  "src/routes/_auth/ec2/(route-tables)/describe-route-tables/$id.tsx",
  "src/routes/_auth/ec2/(target-groups)/-components/describe-target-groups-page.tsx",
  "src/routes/_auth/ecr/(repositories)/-components/lifecycle-policy-editor.tsx",
  "src/routes/_auth/ecr/(repositories)/-components/repositories-page.tsx",
  "src/routes/_auth/ecr/(repositories)/-components/repository-detail-page.tsx",
  "src/routes/_auth/ecs/(clusters)/-components/cluster-detail-page.tsx",
  "src/routes/_auth/ecs/(clusters)/-components/clusters-list-page.tsx",
  "src/routes/_auth/ecs/(clusters)/-components/container-instances-tab.tsx",
  "src/routes/_auth/ecs/(clusters)/-components/service-detail-page.tsx",
  "src/routes/_auth/ecs/(clusters)/-components/services-tab.tsx",
  "src/routes/_auth/ecs/(clusters)/-components/task-definitions-list.tsx",
  "src/routes/_auth/ecs/(clusters)/-components/task-detail-page.tsx",
  "src/routes/_auth/ecs/(clusters)/-components/tasks-tab.tsx",
  "src/routes/_auth/nodes.tsx",
  "src/routes/_auth/rds/(db-instances)/-components/db-instances-list-page.tsx",
  "src/routes/_auth/rds/(parameter-groups)/-components/db-parameter-groups-list-page.tsx",
  "src/routes/_auth/rds/(parameter-groups)/-components/parameters-editor.tsx",
  "src/routes/_auth/rds/(snapshots)/-components/db-snapshots-table.tsx",
  "src/routes/_auth/rds/(subnet-groups)/-components/db-subnet-group-detail-page.tsx",
  "src/routes/_auth/rds/(subnet-groups)/-components/db-subnet-groups-list-page.tsx",
  "src/routes/_auth/rds/-components/rds-events-panel.tsx",
  "src/routes/_auth/route53/(hosted-zones)/-components/list-hosted-zones-page.tsx",
  "src/routes/_auth/s3/-components/service-metrics-page.tsx",
])

describe("route files", () => {
  it("are found", () => {
    expect(Object.keys(sources).length).toBeGreaterThan(100)
  })

  it("add no new hand-written <table>", () => {
    const offenders = Object.entries(sources)
      .filter(
        ([path, source]) =>
          source.includes("<table") && !HAND_WRITTEN_TABLES.has(path),
      )
      .map(([path]) => path)

    expect(offenders).toStrictEqual([])
  })

  it("keep the hand-written table list current", () => {
    const stale = [...HAND_WRITTEN_TABLES].filter(
      (path) => !sources[path]?.includes("<table"),
    )

    expect(stale).toStrictEqual([])
  })
})
