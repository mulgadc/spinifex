import { describe, expect, it } from "vitest"

// Raw source of every route file and shared component, keyed by its path from
// the frontend root.
function byPath(modules: Record<string, string>): Record<string, string> {
  return Object.fromEntries(
    Object.entries(modules).map(([path, source]) => [path.slice(1), source]),
  )
}

const ROUTES = byPath(
  import.meta.glob<string>(["/src/routes/**/*.tsx", "!**/*.test.tsx"], {
    eager: true,
    import: "default",
    query: "?raw",
  }),
)

const ALL = byPath(
  import.meta.glob<string>(
    ["/src/routes/**/*.tsx", "/src/components/**/*.tsx", "!**/*.test.tsx"],
    { eager: true, import: "default", query: "?raw" },
  ),
)

// Everything but the primitives, which are what render the raw elements.
const COMPOSED = Object.fromEntries(
  Object.entries(ALL).filter(
    ([path]) => !path.startsWith("src/components/ui/"),
  ),
)

// The catalogue shows primitives and patterns on their own, so it is exempt.
const CATALOGUE = "src/routes/_auth/-components/design/"

type Allowlist = Record<string, string>

function offenders(
  files: Record<string, string>,
  breaks: (source: string, path: string) => boolean,
  allowed: Allowlist = {},
): string[] {
  return Object.entries(files)
    .filter(
      ([path, source]) =>
        !path.startsWith(CATALOGUE) &&
        !(path in allowed) &&
        breaks(source, path),
    )
    .map(([path]) => path)
}

// An allowed file that no longer breaks the rule, or no longer exists.
function stale(
  files: Record<string, string>,
  breaks: (source: string, path: string) => boolean,
  allowed: Allowlist,
): string[] {
  return Object.keys(allowed).filter((path) => {
    const source = files[path]

    return source === undefined || !breaks(source, path)
  })
}

function has(source: string, pattern: RegExp): boolean {
  return pattern.test(source)
}

const IMPORTS_DIALOG = /"@\/components\/ui\/(?:alert-)?dialog"/u

const FORM_FIELD =
  /<(?:form|Field|Input|Textarea|Select|SelectField|TextField|Switch|SwitchList|TagsFieldArray|FormActions|RadioGroup|GpuInstanceTypeSelect|VpcSelectField|SecurityGroupPicker)\b/u

const DIALOG_FORMS = {
  "src/routes/_auth/rds/(db-instances)/-components/delete-db-instance-dialog.tsx":
    "AWS's own delete dialog asks whether to keep a final snapshot, and its name",
  "src/routes/_auth/ec2/(volumes)/-components/detach-volume-dialog.tsx":
    "AWS's own detach confirmation carries the Force detach option",
  "src/routes/_auth/ec2/-components/private-key-modal.tsx":
    "shows a new private key once, read-only, for the user to copy",
} satisfies Allowlist

function dialogForm(source: string): boolean {
  return has(source, IMPORTS_DIALOG) && has(source, FORM_FIELD)
}

function createRouteFile(path: string): boolean {
  return !path.includes("/-components/") && /\/create-[^/]+\.tsx$/u.test(path)
}

describe("ADR-0016:S3 create flows open in the side panel and dialogs only confirm", () => {
  it("renders no form inside a dialog", () => {
    expect(offenders(ALL, dialogForm, DIALOG_FORMS)).toStrictEqual([])
  })

  it("allows only dialogs that still hold a form", () => {
    expect(stale(ALL, dialogForm, DIALOG_FORMS)).toStrictEqual([])
  })

  // ECS's service and task launches open from their cluster with launch=.
  it("redirects every old create address to its list with the panel open", () => {
    const creates = Object.entries(ROUTES).filter(([path]) =>
      createRouteFile(path),
    )

    const broken = creates
      .filter(
        ([, source]) =>
          !has(source, /\bredirect\(/u) ||
          !has(source, /create: true|launch: "/u),
      )
      .map(([path]) => path)

    expect(creates.length).toBeGreaterThan(20)
    expect(broken).toStrictEqual([])
  })
})

describe("ADR-0016:S4 a submitting form is locked and spins its button", () => {
  it("locks every form that has submit actions", () => {
    expect(
      offenders(
        ALL,
        (source) =>
          has(source, /<FormActions\b/u) && !has(source, /<FormLock\b/u),
      ),
    ).toStrictEqual([])
  })
})

const CHECKBOX_USERS = {
  "src/components/data-table.tsx": "selects table rows",
} satisfies Allowlist

function usesCheckbox(source: string): boolean {
  return (
    has(source, /"@\/components\/ui\/checkbox"/u) ||
    has(source, /type="checkbox"/u)
  )
}

describe("ADR-0016:S5 switches set and checkboxes select rows", () => {
  it("uses a checkbox only to select table rows", () => {
    expect(offenders(ALL, usesCheckbox, CHECKBOX_USERS)).toStrictEqual([])
  })

  it("allows only files that still use a checkbox", () => {
    expect(stale(ALL, usesCheckbox, CHECKBOX_USERS)).toStrictEqual([])
  })
})

describe("ADR-0016:S6 every form shows its AWS CLI equivalent", () => {
  it("shows CLI commands beside every form's submit actions", () => {
    expect(
      offenders(
        ALL,
        (source) =>
          has(source, /<FormActions\b/u) && !has(source, /<CliCommandPanel\b/u),
      ),
    ).toStrictEqual([])
  })
})

describe("ADR-0016:S9 a resource list is a PageHeading over a ResourceView", () => {
  it("heads every list and panel view", () => {
    expect(
      offenders(
        ROUTES,
        (source) =>
          has(source, /<ResourceView\b/u) && !has(source, /<PageHeading\b/u),
      ),
    ).toStrictEqual([])
  })
})

describe("ADR-0016:S10 the open panel is shared URL state", () => {
  it("takes selected, tab and create from panelSearchFields", () => {
    expect(
      offenders(ROUTES, (source) =>
        has(source, /^\s+(?:selected|tab|create): z\./mu),
      ),
    ).toStrictEqual([])
  })
})

const TAB_ID =
  /id: "(?<tab>overview|listeners|attributes|monitoring|settings)"/u

describe("ADR-0016:S12 panel tabs and contents come from the panel system", () => {
  it("puts Overview first", () => {
    const panels = Object.entries(ROUTES).filter(([, source]) =>
      has(source, /tabs=\{|PanelTabSpec/u),
    )

    const wrong = panels
      .filter(([, source]) => {
        const first = TAB_ID.exec(source)?.groups?.tab

        return first !== undefined && first !== "overview"
      })
      .map(([path]) => path)

    expect(panels.length).toBeGreaterThan(20)
    expect(wrong).toStrictEqual([])
  })

  it("never brings back the retired cards", () => {
    expect(
      offenders(ALL, (source) =>
        has(source, /\b(?:DetailCard|DetailRow|ListCard)\b/u),
      ),
    ).toStrictEqual([])
  })
})

const RAW_FIELDS = {
  "src/routes/_auth/s3/-components/upload-button.tsx":
    "the hidden file input behind the Upload button",
  "src/routes/_auth/ec2/(instances)/-components/choice-tile.tsx":
    "a visually hidden radio inside a labelled tile, which the tile draws",
} satisfies Allowlist

function rawField(source: string): boolean {
  return has(source, /<(?:input|select|textarea)\b/u)
}

describe("ADR-0016:S13 forms use the shared fields and primitives", () => {
  it("renders no raw input, select or textarea", () => {
    expect(offenders(COMPOSED, rawField, RAW_FIELDS)).toStrictEqual([])
  })

  it("allows only files that still render one", () => {
    expect(stale(COMPOSED, rawField, RAW_FIELDS)).toStrictEqual([])
  })
})

describe("ADR-0016:S15 errors use the shared error components", () => {
  it("never builds an alert of its own", () => {
    expect(
      offenders(ROUTES, (source) => has(source, /"@\/components\/ui\/alert"/u)),
    ).toStrictEqual([])
  })
})
