# Spinifex UI design system

The console's tokens, primitives and the rules that hold pages to them.
The live catalogue is the `/design` route: every token and primitive below rendered in both themes.
It is open to admins in production and to anyone under `vite dev`.

Source: `spinifex/runtime/roles/spinifexui/frontend/`.
Tokens are in `src/styles.css`, primitives in `src/components/ui/`, and the catalogue in `src/routes/_auth/-components/design/`.

## Principles

- **Pages compose primitives; they do not restyle them.** If a page needs something the system lacks, add it to the system first.
- **Colour is for status.** Surfaces are neutral, and brand orange marks the one primary action on a view.
- **Borders, not shadows.** Overlays (dialogs, sheets, popovers, menus) are the only elevated surfaces.
- **AWS semantics, our presentation.** States, field names and actions follow AWS.

## Tokens

All colours are OKLCH and come from mulgadc.com, so product and site match.

### Surfaces

| Token | Use |
|---|---|
| `background` | The app page. Warm off-white in light, ink `#14181D` in dark. |
| `card`, `popover` | Raised surfaces. White in light, about `#1B2027` in dark. |
| `muted`, `secondary`, `accent` | Hover rows, secondary buttons, quiet fills. |
| `border` | Every divider and card edge. `#DCD8CE` in light, `oklch(1 0 0 / 8%)` in dark. |
| `brand` | Brand orange `#EE4B12`, for the logo and decoration only. It fails text contrast, so it never carries text. |
| `primary` | Darker orange in light, brighter orange in dark: primary button, link, focus ring, active tab. |
| `sidebar` | The navigation column. |
| `console`, `console-foreground` | A serial console log. Light grey on the light theme and near-black on the dark one, so the log follows the theme. |
| `console-stamp`, `console-muted`, `console-ok`, `console-fail` | The log's tones: the kernel timestamp (brand yellow in dark, a darker amber in light), the subsystem or device that logged the line, and systemd's `[ OK ]` and `[FAILED]` markers. |

### Status

Each status has a solid token plus `-foreground` (text on a tinted background), `-subtle` (tinted background) and `-border`.

| Status | Hue | AWS states |
|---|---|---|
| `success` | green | running, available, completed, healthy |
| `warning` | brand yellow `#F5B500` | pending, stopping, creating, modifying, deleting, draining |
| `danger` | cool red, hue 17 | stopped, error, failed, unhealthy, unavailable |
| `info` | blue | informational notes |
| `neutral` | grey `#9AA1A8` | terminated (muted text), and any unmapped state |

`stopped` is danger because EC2 reads it that way, although RDS treats it as a resting state; splitting the map by service is its own change.

`StateBadge` (`src/components/state-badge.tsx`) is the only mapping from an AWS state to a tone, and it exposes the result as `data-tone` for tests.
Every status badge measures above 4.8:1 against its tinted background in both themes.

**Danger is separated from the primary accent by hue alone in dark mode**: the two have the same lightness.
So a destructive action is never a solid danger fill, and a danger state always carries a text label or an icon.

### Chart palette

`--chart-1` to `--chart-8` are categorical, with lightness spread so adjacent series separate under protanopia, deuteranopia and tritanopia.
Measured with a simulated CVD ΔE, the worst pair is 7.4 in light and 7.2 in dark.
Colour is never the only key: label every series.

### Typography

| Role | Font |
|---|---|
| Page titles and section headings (`h1`–`h3`) | Archivo (`font-heading`) |
| UI text, tables and forms | Geist (`font-sans`) |
| IDs, ARNs, IPs, CIDRs, code and eyebrow labels | JetBrains Mono (`font-mono`), with `tabular-nums` for figures |

| Step | Size | Use |
|---|---|---|
| `2xl` | 26px | page title (`PageHeading`) |
| `xl` | 22px | section heading |
| `lg` | 17px | |
| `base` | 15px | |
| `sm` | 14px | body and table default |
| `xs` | 13px | badges, secondary text |
| `2xs` | 12px | mono eyebrow labels |

The scale sits a step above Tailwind's defaults, as consoles such as Vultr's do, because 13px body text read as cramped on a wide screen.
Code blocks are set at 12px (`--text-code-block`) so a long command still fits.

### Primary and contrast

The brand orange cannot be the primary: white on `#EE4B12` is 3.72:1, and as link text on a light card it is also 3.72:1, against WCAG AA's 4.5:1.
So `primary` is its own token, and `brand` keeps the exact site colour for decoration.

| Theme | `primary` | Fill text | Fill contrast | As a link |
|---|---|---|---|---|
| light | `oklch(55% 0.19 36.6)` | white | 5.30:1 | 5.30:1 on a card, 4.69:1 on the page |
| dark | `oklch(68% 0.18 38)` | ink | 5.70:1 | 5.27:1 on a card |

Dark mode puts ink on the fill rather than white, because no orange is both bright enough to read as a link on ink and dark enough to carry white text.

### Radius

`--radius` is 0.5rem; `rounded-md` is that, `rounded-sm` and `rounded-lg` step 2px either side, and status pills are `rounded-full`.

### Themes

The console defaults to dark, and the header switch toggles light and dark (`localStorage["spinifex-ui-theme"]`).
`public/preboot.js` runs before the bundle. It applies the stored theme and `color-scheme` before first paint, so a dark-mode user never sees a white page while the bundle loads, and it turns off zod's eval probe (below).
It is an external file because the console's CSP forbids inline scripts.

## Constraints from the console's CSP

The Go console serves `script-src 'self'; style-src 'self'`.

- **No inline `<script>` or `<style>` elements.** A library that injects either breaks in production while working under `vite dev`, which applies no CSP. This is why `sonner` is not used.
- **No runtime CSS variables set through `style`.** Primitives use fixed class maps instead, as `ToggleGroup` does for `spacing`.
- **Base UI's `ScrollArea` injects a `<style>` element** to hide native scrollbars. `main.tsx` wraps the app in `CSPProvider disableStyleElements`, and `styles.css` carries the rule instead (`.base-ui-disable-scrollbar`).
- **zod compiles validators with `new Function`** unless it is told not to, and it probes for that at module load, before any app code runs. `public/preboot.js` sets `globalThis.__zod_globalConfig = { jitless: true }` first.
- **Chrome reports a violation whenever `DOMParser` meets a body that is not XML**, because the `<parsererror>` element it builds carries an inline style. The AWS SDK parses every query-protocol error body, so a proxy that answers with plain text or HTML produces a console violation as well as the error. Harmless, but it is noise in exactly the place someone debugging a failed call will look.
- Base UI overlays animate through `data-starting-style` and `data-ending-style` transitions, not keyframe animation libraries, and every transition has `motion-reduce:transition-none`.

## Primitives

`src/components/ui/` holds the shadcn primitives on Base UI.
Added in Phase 1: breadcrumb, checkbox, command (`cmdk`), dialog, hover-card, popover, resizable (`react-resizable-panels`), scroll-area, slider, switch, table, toggle and toggle-group.
`button-group` joins buttons edge to edge, as the instance power buttons are.

Every dialog closes on Escape.
⌘↵ or Ctrl+Enter submits it: an `AlertDialog` clicks its action unless that is disabled, and a `Dialog` submits its form, including from a textarea where Enter alone adds a line.
Enter in a typed confirmation confirms once the phrase matches.
The confirm button shows the shortcut, ⌘↵ on a Mac and Ctrl ↵ elsewhere (`src/lib/shortcuts.ts`), and names it in `aria-keyshortcuts`.

A disabled control stays visible and shows `cursor: not-allowed`, so it says why it does nothing rather than vanishing.
An on/off setting is a `Switch`, orange when on; a `Checkbox` only picks items from a list, such as table rows, subnets or security groups.

`InfoTip` (`src/components/info-tip.tsx`) is UniFi's (i): a small button beside a label that opens a short explanation on hover or click, with `InfoList` for the facts inside it.
`PanelField` takes it through `info`, so the login user names the image's distribution and the image names its AMI details.

Base UI marks orientation with `data-orientation`, so `styles.css` defines the `data-horizontal` and `data-vertical` variants the shadcn sources expect.

Base UI marks the selected tab with `data-active`, not `data-selected`, so `ui/tabs` styles that attribute.

shadcn's `chart` is not used: it pulls `recharts`, whose `reselect@5.1.1` dependency fails the workspace's `trustPolicy: no-downgrade`.
Charts are uPlot instead (see Charts).

## Enforcement

Four lint layers keep pages on the system.

| Rule | Where | What it catches |
|---|---|---|
| `shadcn/no-unknown-classes` | all source, not tests | a class Tailwind does not generate, such as a typo or a removed token |
| `shadcn/no-restyle` | all source | a page passing layout or colour classes to a primitive |
| `eslint/no-restricted-imports` | `src/routes/**` | a route importing a chart library directly |
| `src/routes/route-files.test.ts` | test suite | a new hand-written `<table>` in a route |

The table test is a ratchet: `HAND_WRITTEN_TABLES` lists the routes that still have one, it fails when a new file adds one, and it fails when a listed file no longer has one so the list only shrinks.

## Shell

`src/lib/nav.ts` is the one description of the console's navigation.
The sidebar, the command palette and the breadcrumbs all read it, and `nav.test.ts` checks every destination against the generated route tree, so a renamed route fails a test rather than leaving a dead link.

- **Sidebar.** Groups take generic names rather than AWS's service marks: Compute, Databases, Authentication and Object Storage, not EC2, RDS, IAM and S3. Pages and the rest of the console keep the AWS names, and each group's `service` keeps its AWS name findable in the command palette, where typing it exactly ranks that group first. Each service group folds, and a folded group stays folded across reloads (`localStorage["spinifex-ui-nav-collapsed"]`). Navigating into a folded group unfolds it so the active item shows, and it can then be folded again, the current group included. On a phone the sidebar is a sheet, and any navigation closes it.
- **Command palette.** ⌘K or Ctrl K, or the Search button in the header. It lists every page the user can reach, every create flow (`create` on a nav item), and a theme toggle. It is navigation only, with no resource search yet.
- **Breadcrumbs.** Service, then list page, then the resource or the create flow. A resource ID is set in mono. On a phone only the last crumb shows.
- **Sign in.** The sign-in card sits over the shell on a light, barely blurred overlay rather than on a page of its own, so the console stays in view. The shell behind it is `inert`, which keeps pointer, keyboard and screen reader out of it until the user signs in.
Signing in does not reload the page: the card shrinks and fades with its veil over 300ms, then the console routes to the dashboard in place, and the page content fades in. Signing out is a 300ms view-transition cross-fade into the sign-in card, which says "Signed out." in a warning-coloured notice. Under reduced motion both switch at once.

## Layout

The app shell's `<main>` is `min-w-0`, so a wide table scrolls inside its card instead of widening the page.
The page gutter is 16px on a phone, 24px from `md` and 32px from `xl`.
`PageHeading` wraps its actions below the title when they do not fit beside it.
Every page must fit at 390px with `document.documentElement.scrollWidth` equal to the window width.

## Data tables

`DataTable` (`src/components/data-table.tsx`) renders `@tanstack/react-table` over the `ui/table` primitives, so sorting, row selection and shift-click range selection are the library's and not ours to maintain.
Pages describe columns with the small `DataColumn` shape (`id`, `header`, `cell`, `sortValue`, `hideBelow`, `align`), which the component maps to TanStack column definitions; a column with a `sortValue` sorts, with missing values last and numbers compared as numbers.
Sort and selection stay controlled by the caller, so a page can keep them in the URL.

A column hides itself through `hideBelow`, which is a **container** query on the table's card (`@max-lg`, `@max-xl`, `@max-4xl`, `@max-5xl`), not a viewport one.
So columns drop out when a side panel narrows the list as well as on a small screen.

List pages keep their filter, state and sort in the URL, so a filtered view can be bookmarked, shared and survives a reload.
EC2 Instances is the reference page (`ec2/(instances)/-components/instances-page.tsx`):

- The first column names the resource: the `Name` tag in the primary colour, with the ID in mono beneath it, opening its side panel.
- Bulk actions appear above the table once a row is selected. Each action counts only the selected instances it applies to, so a mixed selection never sends a call that must fail, and Terminate asks first and lists every ID.
- An empty account gets a call to action, and an empty filter result says the filters are why.

VPCs and Internet gateways follow the same pattern.

### Selecting rows to delete

Every list with a delete action has row checkboxes. Selecting a row shows `SelectionBar` (`src/components/selection-bar.tsx`) above the table, with the count, the actions and a way to clear, and `useRowSelection` (`src/hooks/use-row-selection.ts`) keeps the selection to rows still in the list.
`BulkDelete` (`src/components/bulk-delete.tsx`) is the bar's destructive action, and its dialog names every selected resource before anything is sent.

- **One call per resource, one at a time.** Most AWS delete calls take a single ID, so each refusal lands against its own row with AWS's message. A partial failure keeps the dialog open, and its confirm becomes "Retry N" for only the ones that failed.
- **A batch call where AWS has one.** `deleteMany` instead of `deleteOne` hands the whole selection to one function that returns the refusals by ID, as S3's `DeleteObjects` does for up to 1000 keys.
- **No pre-filtering.** The bar sends what was selected and lets AWS refuse, rather than guessing which deletes would fail. Instances are the exception, since one call takes many IDs and a mixed selection would fail as a whole.
- **The bulk delete and the panel's delete are the same function.** Where a single delete is several calls, such as removing dependants first, the sequence lives in one place and both use it.
- The verb follows the AWS console: Release for an Elastic IP, Deregister for an image.

### Typed confirmation

A delete that cannot be walked back asks the user to type a word first, as the AWS console does before deleting a volume.
`ConfirmPhrase` (`src/components/confirm-phrase.tsx`) is the field, and the confirm stays disabled until it matches, ignoring case and surrounding space.
`DeleteConfirmationDialog`, `ResourceDeleteDialog` and `BulkDelete` take it through `confirmPhrase`, so the panel's delete and the bulk delete ask the same way.

- **"delete"** for volumes, snapshots, RDS instances and snapshots, EKS and ECS clusters and Route 53 hosted zones. An RDS instance deleted without a final snapshot asks for its identifier instead, which is stronger.
- **"terminate"** for instances.
- Not for anything cheap to recreate, such as a key pair, a security group rule or a tag.

Key pairs is the smallest example (`ec2/(key)/-components/key-pairs-page.tsx`).

### Object browser

A bucket opens as a page rather than a panel, since its contents need the width (`s3/-components/object-browser.tsx`).
Folders and objects share one table, folders first as S3 lists them; a folder row enters the folder, and an object row opens its panel with `HeadObject`'s properties, its user metadata and a download.

- **The find box is S3's prefix.** It sends `Prefix` = folder + typed text to `ListObjectsV2` after a 250ms pause, so it finds keys the current page has not loaded, and it matches from the start of the name only.
- **Pages are S3's.** 100 entries a page with `MaxKeys`, and Next follows `NextContinuationToken`. Tokens only run forward, so the browser keeps the stack of tokens that opened each page, and Previous pops it; sorting a column sorts the page on screen.
- **Selecting a folder selects everything under it.** Delete lists every key under the folder, then sends `DeleteObjects` in batches of 1000, and a folder that was only partly deleted says how much was left.
- The bucket list deletes only empty buckets; S3 refuses the rest with `BucketNotEmpty`, and the dialog says so before anything is sent.

#### Uploads

Uploads run in a store outside React (`src/lib/uploads.ts`), so they carry on, and keep their progress, while the user browses elsewhere in the console.

- **A small, quick upload stays on the button.** While a batch runs, Upload becomes "Uploading N%" with a ring that fills, and a click on it opens the progress dialog.
- **A large or slow batch opens the dialog.** It opens at once for 16 MiB or more, and otherwise after 2 seconds if the batch is still going, so a quick upload never flashes a dialog. Hide keeps the batch on the button, and the dialog does not reopen on its own after that.
- **The dialog shows what a user would ask about.** Bytes sent of the total, the rate in bits a second with SI prefixes (kbit/s, Mbit/s, Gbit/s) over the last 3 seconds, an approximate time left, and a row per file with its own progress and cancel. A rate that has fallen to nothing after bytes moved reads "Stalled". A finished batch gives its elapsed time and average rate, and failures can be retried.
- **Files go three at a time, each in 8 MiB parts sent four at a time**, with lib-storage's `Upload` over `XhrHttpHandler`, since `fetch` reports no upload progress. Each file has its own client, as the handler's progress event does not say which request it came from.
- Leaving the page while uploads run asks first, and signing out cancels them.
- The console's S3 proxy gives each request an hour, in place of the server's 15-second read and 120-second write timeouts, which would cut off a part or a download on a slow link.

### Instance launch

The image picker shows each distribution's own mark from `src/assets/os/` (Ubuntu, Debian, Rocky, Alpine, CentOS), imported with `?no-inline` so it is served as a file and satisfies `img-src 'self'`. Any other distribution gets a generic server icon, and the "Other images" tile a disc.
The instance type select states the chosen type's vCPUs and memory beneath it, from `DescribeInstanceTypes`.

## Side panels

Modelled on UniFi's console: a click on a row opens the resource in a panel beside the list, and a create flow opens there too.

- **URL.** The panel is search state on the list route: `selected=<id>`, `tab=overview|monitoring|settings`, and `create=true`. Defaults are stripped, so a closed panel leaves the plain list address. Opening or closing a panel pushes a history entry, so Back closes it; a tab or filter change replaces the entry.
- **Layout.** From `lg` (1024px) the panel is fixed to the right edge, full height below the header, flush with no outer margin, and it slides in over the list. The list keeps its width and columns underneath, so nothing reflows when a panel opens, and a soft gradient on the panel's left edge reads as the panel lying on top. Below `lg` there is no room to share, so the panel replaces the list as a full page with a back arrow. `useWideLayout` decides, through `matchMedia`.
- **`DetailPanel`.** A header with the resource's own page (left) and close (right), the title and its ID, a summary of state and quick actions, then icon tabs with tooltips. Escape closes it unless a dialog or menu is open over it. Closing slides the panel out over 150ms, or at once under reduced motion. Each tab, and a create form, sits in its own error boundary, so a failed query shows in the panel and the list keeps working.
- **Contents.** `PanelSection` is a folding card, `PanelFields` and `PanelField` a label and value list, with `copy` for a one-click copy of IDs and addresses, and `PanelTags` the tag list. Create forms take an `onClose(id?)` prop: embedded, they hide their back link and heading, and a create selects the new resource.
- **One component is both the panel and the page.** Each resource has a single details component (`InstanceDetails`, `VpcDetails`, `InternetGatewayDetails`) that takes `layout="auto" | "page"`. The list renders it as the panel; `/ec2/describe-instances/$id` and its kind render the same component as a page, as one centred column under a standard left-aligned heading, with state and actions on the right and text tabs. A feature added to the panel is on the page by construction, and the page ignores Escape and has a back arrow in place of close. Expanding the panel to its page keeps the tab, and query results such as fetched console output come along from the cache.
- **A click on the list's background closes the panel**, as UniFi's does. A row selects another resource instead, and links, buttons and inputs keep their own behaviour. Selecting rows for a bulk action closes it too, so the bulk bar and the panel never compete.
- **Actions.** State actions sit in the summary as a button group, and the rest go in an Actions menu, Unikraft style. Stop, Reboot and every destructive action confirm in a dialog.
- **Loading.** A panel or page still loading shows `PanelSpinner`, a centred spinner on the background, not a grey skeleton block.
- **`DetailCard`, `DetailRow` and `ListCard` are retired.** Every resource page uses the panel sections instead.

## Charts

`MetricChart` (`src/components/charts/metric-chart.tsx`) draws a time series with uPlot, chosen for size and because it draws on a canvas, which the CSP allows where inline style attributes are not; `csp.e2e.ts` renders one under the production header.
The look follows UniFi's statistics: smoothed lines over a fading fill, a light grid, a legend chip per series that reads the value under the cursor (or the latest), and `-24 hrs … Now` beneath.
Series colours are the chart palette, and the chart redraws when the theme changes.

Instance monitoring shows EC2's agentless basic metrics live: CPU utilisation, network bytes in and out, and packets in and out, one sample a second over the last five minutes, with no history to range over.
The chart is built once and fed with `setData`, so a second's tick does not rebuild it, and polling stops while the tab is hidden.
**The data is simulated** (`src/lib/simulated-metrics.ts`, seeded by instance ID and second) and the panel says so with a badge, until awsgw serves `GetMetricData` at a one-second period.

## Dates

`formatDateTime` writes the AWS console's form in the viewer's zone, `2026/06/04 02:00 GMT-5`, with a 24-hour clock and UTC as plain `GMT`.

## Testing

`make test-ui-e2e` rebuilds `dist` and runs `pnpm e2e`, which drives Playwright against two servers (`playwright.config.ts`):

- **dev** (Vite on 3101) for behaviour: the `/design` catalogue in both themes and four widths, theme bootstrap, primitives' keyboard behaviour, the shell, and the Instances, VPC and Internet gateway list and panel flows.
- **preview** (the built `dist` on 3102) for `csp.e2e.ts`, which serves the console's own CSP header and fails on any violation. Vite dev applies no CSP, so this is the only place an inline style or `eval` is caught before production.

`e2e/console.ts` is the fixture. It seeds a fake session and answers `/proxy/**` by action, with empty defaults for what the dashboard and shell ask for; a test adds handlers for its own data, and any call nobody answered fails the test through `api.unhandled`.
Unhandled calls get an error in the protocol's own shape, since a plain-text body would itself trip the CSP test.

### Screenshots

Screenshot baselines live in `e2e/__screenshots__/` and are Chromium renders taken in `en-AU` and UTC, so dates do not move with the host.
They are drawn inside Playwright's own container image, because font rasterisation differs between hosts and a baseline drawn on a workstation fails on the runner.
`playwright.image` in the frontend directory pins that image by version and digest, and is the one place it is named.
Its version must equal the lockfile's `@playwright/test`, since the image carries the browsers for exactly one release, so bump both together.

- `make ui-baselines` redraws every baseline in the pinned image, with `pnpm e2e --update-snapshots=all`, since the default mode keeps any baseline within the diff tolerance and so would keep one drawn on another host. Commit the changed files under `e2e/__screenshots__/` with the change that moved them.
- `make ui-e2e-container` runs the suite in the same image and compares the screenshots, exactly as CI does.

Both run `scripts/ui-playwright.sh`, which uses podman, or docker where podman is absent, and `CONTAINER_RUNTIME` overrides the choice.
It mounts the frontend directory, installs with `pnpm install --frozen-lockfile` into a separate `node_modules` under `~/.cache/spinifex-ui-playwright`, so the host's `node_modules` is never touched, then builds `dist` and runs the suite.
It refuses to run when the image's Node differs from `.nvmrc` or its Playwright differs from the lockfile's.
`make test-ui-e2e` and `pnpm e2e` still run on the host, where screenshots compare reliably only against baselines drawn on that host, so do not commit baselines from `pnpm e2e:update`.

The `UI End-to-End` job in `.github/workflows/ui.yml` runs the suite inside the pinned image on every pull request touching the frontend, and screenshots gate the merge with every behavioural and CSP test.
A preceding `UI End-to-End Image` job reads `playwright.image`, so the workflow and the `make` targets cannot disagree about the image.
On a failure the job uploads `test-results/` as the `ui-e2e-test-results` artifact, holding each failed screenshot's expected, actual and diff images and the trace.

## Open

- **Monitoring data is simulated.** Real samples need a source in the daemon; the plan is `docs/development/feature/ec2-basic-metrics.md` in the mulga repository.
