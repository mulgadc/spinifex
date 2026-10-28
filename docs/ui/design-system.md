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
| `primary` | Brand orange `#EE4B12`: primary button, link, focus ring, active tab. |
| `sidebar` | The navigation column. |

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
| `2xl` | 24px | page title (`PageHeading`) |
| `xl` | 20px | section heading |
| `lg` | 16px | |
| `base` | 14px | |
| `sm` | 13px | body and table default |
| `xs` | 12px | badges, secondary text |
| `2xs` | 11px | mono eyebrow labels |

### Radius

`--radius` is 0.5rem; `rounded-md` is that, `rounded-sm` and `rounded-lg` step 2px either side, and status pills are `rounded-full`.

### Themes

The console defaults to dark, and the header switch toggles light and dark (`localStorage["spinifex-ui-theme"]`).
`public/theme-init.js` applies the stored theme and `color-scheme` before first paint, so a dark-mode user never sees a white page while the bundle loads.
It is an external file because the console's CSP forbids inline scripts.

## Constraints from the console's CSP

The Go console serves `script-src 'self'; style-src 'self'`.

- **No inline `<script>` or `<style>` elements.** A library that injects either breaks in production while working under `vite dev`, which applies no CSP. This is why `sonner` is not used.
- **No runtime CSS variables set through `style`.** Primitives use fixed class maps instead, as `ToggleGroup` does for `spacing`.
- Base UI overlays animate through `data-starting-style` and `data-ending-style` transitions, not keyframe animation libraries, and every transition has `motion-reduce:transition-none`.

## Primitives

`src/components/ui/` holds the shadcn primitives on Base UI.
Added in Phase 1: breadcrumb, checkbox, command (`cmdk`), dialog, hover-card, popover, resizable (`react-resizable-panels`), scroll-area, slider, switch, table, toggle and toggle-group.

Base UI marks orientation with `data-orientation`, so `styles.css` defines the `data-horizontal` and `data-vertical` variants the shadcn sources expect.

There is no chart primitive yet: shadcn's `chart` pulls `recharts`, whose `reselect@5.1.1` dependency fails the workspace's `trustPolicy: no-downgrade`.

## Enforcement

Four lint layers keep pages on the system.

| Rule | Where | What it catches |
|---|---|---|
| `shadcn/no-unknown-classes` | all source, not tests | a class Tailwind does not generate, such as a typo or a removed token |
| `shadcn/no-restyle` | all source | a page passing layout or colour classes to a primitive |
| `eslint/no-restricted-imports` | `src/routes/**` | a route importing a chart library directly |
| `src/routes/route-files.test.ts` | test suite | a new hand-written `<table>` in a route |

The table test is a ratchet: `HAND_WRITTEN_TABLES` lists the routes that still have one, it fails when a new file adds one, and it fails when a listed file no longer has one so the list only shrinks.

## Layout

The app shell's `<main>` is `min-w-0`, so a wide table scrolls inside its card instead of widening the page.
Every page must fit at 500px with `document.documentElement.scrollWidth` equal to the window width.

## Open

- **Brand orange fails text contrast.** White on `#EE4B12` is 3.72:1 and orange link text on a light card is 3.72:1, against WCAG AA's 4.5:1; a dark card gives 4.40:1. A light primary at 56–57% lightness reaches about 5:1 for both uses. In dark mode fills and links pull in opposite directions, so the choice is ink text on a brighter orange (4.75:1) or a separate link token.
- **ToggleGroup's pressed state is weak** in both themes.
