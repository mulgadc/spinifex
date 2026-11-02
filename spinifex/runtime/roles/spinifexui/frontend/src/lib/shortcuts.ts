export const IS_MAC =
  typeof navigator !== "undefined" &&
  /Mac|iPhone|iPad/u.test(navigator.platform)

// Confirms a dialog. Windows and Linux use Ctrl where a Mac uses ⌘.
export const CONFIRM_SHORTCUT = IS_MAC ? "⌘↵" : "Ctrl ↵"
export const CONFIRM_KEYS = IS_MAC ? "Meta+Enter" : "Control+Enter"
