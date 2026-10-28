// Applies the stored theme before first paint, so the page is never white while
// the app bundle loads. Mirrors ThemeProvider's key and its "dark" default; an
// external file because the console's CSP forbids inline scripts.
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
