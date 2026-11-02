import { describe, expect, it } from "vitest"

// Raw source of every route file, keyed by its path from the frontend root.
const SOURCES = Object.fromEntries(
  Object.entries(
    import.meta.glob<string>(["/src/routes/**/*.tsx", "!**/*.test.tsx"], {
      eager: true,
      import: "default",
      query: "?raw",
    }),
  ).map(([path, source]) => [path.slice(1), source]),
)

// Route files that build a table by hand, from a raw <table> or the ui/table
// primitives, each for a stated reason. A list goes through DataTable instead.
const ALLOWED = {
  "src/routes/_auth/-components/design/data-section.tsx":
    "the catalogue's specimen of the ui/table primitives DataTable is built on",
  "src/routes/_auth/rds/(parameter-groups)/-components/parameters-editor.tsx":
    "an editable grid: each row carries its own value and apply-method controls",
} satisfies Record<string, string>

function buildsTableByHand(source: string): boolean {
  return source.includes("<table") || source.includes(`"@/components/ui/table"`)
}

describe("route files", () => {
  it("are found", () => {
    expect(Object.keys(SOURCES).length).toBeGreaterThan(100)
  })

  it("build no table by hand outside the allowlist", () => {
    const offenders = Object.entries(SOURCES)
      .filter(
        ([path, source]) => buildsTableByHand(source) && !(path in ALLOWED),
      )
      .map(([path]) => path)

    expect(offenders).toStrictEqual([])
  })

  it("allow only files that exist and still build a table by hand", () => {
    const stale = Object.keys(ALLOWED).filter((path) => {
      const source = SOURCES[path]

      return source === undefined || !buildsTableByHand(source)
    })

    expect(stale).toStrictEqual([])
  })
})
