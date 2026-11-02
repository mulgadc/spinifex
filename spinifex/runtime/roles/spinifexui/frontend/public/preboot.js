// Runs before the app bundle as a classic script: an external file, because
// the console's CSP forbids inline scripts.

// Applies the stored theme before first paint, so the page is never white while
// the app bundle loads. Mirrors ThemeProvider's key and its "dark" default.
{
  let theme = "dark"

  try {
    theme = localStorage.getItem("spinifex-ui-theme") || theme
  } catch {
    // localStorage might be disabled
  }

  if (theme === "system") {
    theme = matchMedia("(prefers-color-scheme: dark)").matches
      ? "dark"
      : "light"
  }

  theme = theme === "light" ? "light" : "dark"
  document.documentElement.classList.add(theme)
  document
    .querySelector('meta[name="color-scheme"]')
    ?.setAttribute("content", theme)
}

// zod probes for new Function when its first schema is defined, which happens
// at module load, before any app code can call z.config(). The console's CSP
// has no 'unsafe-eval', so the probe logs a violation; zod reads this first.
globalThis.__zod_globalConfig = { jitless: true }
