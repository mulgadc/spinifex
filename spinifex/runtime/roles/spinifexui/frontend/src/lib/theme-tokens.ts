// The console's built-in theme tokens as #RRGGBB, for the Settings page's
// colour pickers and its preview of each theme.

export type ThemeName = "light" | "dark"

interface Rgb {
  r: number
  g: number
  b: number
}

interface OklchToken {
  l: number
  c: number
  h: number
}

export type ThemeToken =
  | "--background"
  | "--card"
  | "--foreground"
  | "--primary"
  | "--primary-foreground"

// The oklch() declarations in styles.css the preview draws each theme with.
export const THEME_TOKENS: Record<ThemeName, Record<ThemeToken, OklchToken>> = {
  light: {
    "--background": { l: 0.958, c: 0.007, h: 88.6 },
    "--card": { l: 1, c: 0, h: 0 },
    "--foreground": { l: 0.211, c: 0.012, h: 254.1 },
    "--primary": { l: 0.55, c: 0.19, h: 36.6 },
    "--primary-foreground": { l: 1, c: 0, h: 0 },
  },
  dark: {
    "--background": { l: 0.207, c: 0.012, h: 254.1 },
    "--card": { l: 0.242, c: 0.015, h: 256.8 },
    "--foreground": { l: 0.93, c: 0.006, h: 250 },
    "--primary": { l: 0.68, c: 0.18, h: 38 },
    "--primary-foreground": { l: 0.211, c: 0.012, h: 254.1 },
  },
}

const HEX_COLOR = /^#[\dA-Fa-f]{6}$/u

export function isHexColor(value: string): boolean {
  return HEX_COLOR.test(value)
}

function encode(v: number): number {
  const x = Math.min(Math.max(v, 0), 1)

  return x <= 0.0031308 ? 12.92 * x : 1.055 * x ** (1 / 2.4) - 0.055
}

function oklch({ l, c, h }: OklchToken): Rgb {
  const hue = (h * Math.PI) / 180
  const a = c * Math.cos(hue)
  const b = c * Math.sin(hue)
  const lp = l + 0.3963377774 * a + 0.2158037573 * b
  const mp = l - 0.1055613458 * a - 0.0638541728 * b
  const sp = l - 0.0894841775 * a - 1.291485548 * b
  const l3 = lp ** 3
  const m3 = mp ** 3
  const s3 = sp ** 3

  return {
    r: encode(4.0767416621 * l3 - 3.3077115913 * m3 + 0.2309699292 * s3),
    g: encode(-1.2684380046 * l3 + 2.6097574011 * m3 - 0.3413193965 * s3),
    b: encode(-0.0041960863 * l3 - 0.7034186147 * m3 + 1.707614701 * s3),
  }
}

function hexByte(v: number): string {
  return Math.round(Math.min(Math.max(v, 0), 1) * 255)
    .toString(16)
    .padStart(2, "0")
}

// A theme token as #RRGGBB, for a colour picker or a preview of the other theme.
export function tokenHex(theme: ThemeName, token: ThemeToken): string {
  const { r, g, b } = oklch(THEME_TOKENS[theme][token])

  return `#${hexByte(r)}${hexByte(g)}${hexByte(b)}`.toUpperCase()
}
