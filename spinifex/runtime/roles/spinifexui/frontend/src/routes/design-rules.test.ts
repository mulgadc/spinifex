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
  "src/components/choice-tile.tsx":
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

// Rules below are named, not numbered, until the amendment that adds them to
// ADR-0016 is accepted.

const TINTED_ROUND = {
  "src/routes/_auth/route53/(hosted-zones)/-components/create-hosted-zone-page.tsx":
    "numbered steps and a success callout, neither of them a status pill",
} satisfies Allowlist

// A pill tinted with a status colour is a status, so it is a StatusBadge.
function handBuiltStatusPill(source: string, path: string): boolean {
  return (
    path !== "src/components/state-badge.tsx" &&
    has(source, /\brounded-full\b/u) &&
    has(
      source,
      /\bbg-(?:success|warning|caution|danger|destructive)(?:\/|-subtle)/u,
    )
  )
}

describe("status pills come from StateBadge or StatusBadge", () => {
  it("builds no status pill of its own", () => {
    expect(
      offenders(COMPOSED, handBuiltStatusPill, TINTED_ROUND),
    ).toStrictEqual([])
  })

  it("allows only files that still tint a rounded element", () => {
    expect(stale(COMPOSED, handBuiltStatusPill, TINTED_ROUND)).toStrictEqual([])
  })

  it("never shows a status in the brand-coloured default Badge", () => {
    expect(
      offenders(COMPOSED, (source) =>
        has(source, /<Badge(?:\s+variant="default")?\s*>/u),
      ),
    ).toStrictEqual([])
  })
})

function dropsTab(source: string, path: string): boolean {
  if (path === "src/components/detail-panel.tsx") {
    return false
  }

  return source
    .split("fullPage={")
    .slice(1)
    .some((after) => !after.split("</Link>")[0]?.includes("search={{ tab }}"))
}

describe("expanding a panel keeps its tab", () => {
  it("passes the open tab to every full-page link", () => {
    const panels = Object.values(ALL).filter((source) =>
      source.includes("fullPage={"),
    )

    expect(panels.length).toBeGreaterThan(20)
    expect(offenders(ALL, dropsTab)).toStrictEqual([])
  })
})

describe("resource tags are edited in place with InlineTags", () => {
  it("never brings back the retired tag panels", () => {
    expect(
      offenders(ALL, (source) => has(source, /\b(?:PanelTags|TagsEditor)\b/u)),
    ).toStrictEqual([])
  })
})

const FILTER_TOGGLES = {
  "src/components/filter-bar.tsx": "the filter bar is the one toggle group",
  "src/routes/_auth/nodes/topology.tsx":
    "the topology toggle picks a layout direction and filters nothing",
} satisfies Allowlist

function usesToggleGroup(source: string): boolean {
  return has(source, /"@\/components\/ui\/toggle-group"/u)
}

describe("lists filter through one FilterBar", () => {
  it("builds no filter toggles of its own", () => {
    expect(offenders(COMPOSED, usesToggleGroup, FILTER_TOGGLES)).toStrictEqual(
      [],
    )
  })

  it("allows only files that still use a toggle group", () => {
    expect(stale(COMPOSED, usesToggleGroup, FILTER_TOGGLES)).toStrictEqual([])
  })
})

// With the React compiler on, a child handed a form never re-renders when
// form.formState changes, and unit tests run with the compiler off.
const OWNS_FORM_TOO = {
  "src/components/elbv2/listener-rule-form.tsx":
    "the rule form owns its useForm; only ConditionRow is handed it",
} satisfies Allowlist

function readsHandedFormState(source: string): boolean {
  return (
    has(source, /UseFormReturn|ReturnType<typeof useForm/u) &&
    has(source, /\bform\.formState\./u)
  )
}

describe("a component handed a form reads errors through useFormState", () => {
  it("never reads formState from a form prop", () => {
    expect(
      offenders(COMPOSED, readsHandedFormState, OWNS_FORM_TOO),
    ).toStrictEqual([])
  })

  it("allows only files that still read it", () => {
    expect(stale(COMPOSED, readsHandedFormState, OWNS_FORM_TOO)).toStrictEqual(
      [],
    )
  })
})

// A fill of the page's text colour turns white on the dark theme.
function invertsTheSurface(source: string): boolean {
  return has(source, /(?<![\w:-])(?:bg-foreground|text-background)(?![\w/-])/u)
}

const FLOATING = [
  "src/components/ui/tooltip.tsx",
  "src/components/ui/popover.tsx",
  "src/components/ui/hover-card.tsx",
  "src/components/ui/dropdown-menu.tsx",
  "src/components/ui/select.tsx",
  "src/components/ui/combobox.tsx",
]

describe("floating surfaces follow the theme", () => {
  it("never fills a surface with the inverted text colour", () => {
    expect(offenders(ALL, invertsTheSurface)).toStrictEqual([])
  })

  it("draws every tooltip, popover and menu on the popover tokens", () => {
    expect(
      FLOATING.filter(
        (path) =>
          !ALL[path]?.includes("bg-popover") ||
          !ALL[path].includes("text-popover-foreground"),
      ),
    ).toStrictEqual([])
  })
})
