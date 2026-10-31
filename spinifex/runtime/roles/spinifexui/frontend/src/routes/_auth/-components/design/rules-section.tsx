import { CatalogueSection } from "./catalogue-section"

// The design-system proposal's clauses, in brief. The proposal is the source;
// this is what a contributor checks a change against while building it.
const RULES = [
  "Build from what is on this page. A new shared component or pattern is added here in the same change, and a unit test fails if one is missing.",
  "Create flows open in the side panel over their list. Dialogs confirm; they never create.",
  "Submitting spins the button in place and locks the form. A failure shows beside the button and releases the form. Nothing above the form appears or moves.",
  "Settings that are on or off are switches, and multi-select pickers are boxed lists of switches. Checkboxes only select table rows.",
  "Every create form shows the AWS CLI commands that do the same.",
  "Colours, type and spacing come from the tokens below. Primitives are not restyled where they are used, and every component works in both themes.",
  "Appliance images are installed from the console by the management account only; every other account is told its operator must install them.",
]

export function RulesSection() {
  return (
    <CatalogueSection
      description="Proposed as PROP-CONSOLEUI-001, in docs/development/proposals/console-design-system/. This page is served by the Vite dev server only, and is absent from release builds."
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
