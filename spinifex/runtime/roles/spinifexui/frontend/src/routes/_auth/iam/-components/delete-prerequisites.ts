export type IamDeleteKind =
  | "user"
  | "group"
  | "role"
  | "policy"
  | "instance profile"

// What AWS refuses to delete over, as [one, many], so the panel's dialog and
// the bulk bar's name the same dependants.
const PREREQUISITES: Record<IamDeleteKind, readonly [string, string]> = {
  user: [
    "Detach its policies, delete its access keys and remove it from its groups first",
    "Detach their policies, delete their access keys and remove them from their groups first",
  ],
  group: [
    "Remove its members and detach its policies first",
    "Remove their members and detach their policies first",
  ],
  role: [
    "Detach its policies and remove it from its instance profiles first",
    "Detach their policies and remove them from their instance profiles first",
  ],
  policy: [
    "Detach it from every user, group and role first",
    "Detach them from every user, group and role first",
  ],
  "instance profile": ["Remove its role first", "Remove their roles first"],
}

export function iamDeleteWarning(kind: IamDeleteKind, count = 1): string {
  const [one, many] = PREREQUISITES[kind]

  return `${count === 1 ? one : many}, or the delete is refused. This cannot be undone.`
}
