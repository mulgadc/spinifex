import { CatalogueSection } from "./catalogue-section"

// ADR-0016's clauses, in brief. The ADR is the source; this is what a
// contributor checks a change against while building it.
const RULES = [
  "Build from what is on this page. A new shared component or pattern is added here in the same change, and a unit test fails if one is missing.",
  "Create flows open in the side panel over their list. Dialogs confirm; they never create.",
  "Submitting spins the button in place and locks the form. A failure shows beside the button and releases the form. Nothing above the form appears or moves.",
  "Settings that are on or off are switches, and multi-select pickers are boxed lists of switches. Checkboxes only select table rows.",
  "Every create form shows the AWS CLI commands that do the same.",
  "Colours, type and spacing come from the tokens below. Primitives are not restyled where they are used, and every component works in both themes.",
  "Appliance images are installed from the console by the management account only; every other account is told its operator must install them.",
  "Every list page is a page heading over a resource view and a data table.",
  "Clicking a row opens its details in the side panel, and the address records the selection, tab and create form, so Back closes the panel.",
  "One details component serves both the side panel and the full page.",
  "Panel tabs come from the shared tab list with Overview first, and their content is built from panel sections and fields.",
  "Forms use the shared fields or the ui primitives, never raw inputs.",
  "Rows are deleted through the selection bar, with a typed phrase when the delete cannot be undone.",
  "A page that fails to load shows an error banner.",
  "A pattern that is missing is added to the shared components, this page and the design-system doc before it is used.",
]

export function RulesSection() {
  return (
    <CatalogueSection
      description="Decided in ADR-0016, docs/adr/0016-console-design-system.md in the mulga repository. This page is served by the Vite dev server only, and is absent from release builds."
      title="Rules"
    >
      <ol className="flex list-decimal flex-col gap-2 rounded-lg border bg-card p-4 pl-9">
        {RULES.map((rule) => (
          <li key={rule}>{rule}</li>
        ))}
      </ol>
    </CatalogueSection>
  )
}
