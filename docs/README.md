# Spinifex Documentation

This directory contains the Spinifex documentation. This is available in individual markdown files, and a generated website to aid navigation and browsing documentation at: [https://docs.mulgadc.com](https://docs.mulgadc.com/)

The [AWS API coverage](coverage/README.md) pages are generated from Spinifex's dispatch tables and the pinned AWS API models, one page per service. Run `make aws-model-coverage` to regenerate them and print a summary. Only `pages.json` and the `intro.md` files under `coverage/` are hand-written; every `README.md` there is generated and will be overwritten.

The same run writes [`coverage/coverage.json`](coverage/coverage.json), a versioned machine-readable model-versus-dispatch inventory. It is generated and must not be edited. Its operation states are structural: `implemented` means a registered action has neither a stub nor an explicit refusal. It is not a behavioural compatibility claim; conformance evidence and capability profiles establish that separately.

A published page names the operations Spinifex serves, and nothing else. It carries no percentage, no unimplemented operation and no operation the platform will never offer: the same run writes those to [`coverage/internal-report.md`](coverage/internal-report.md), which no slug in the docs site's configuration points at, so it stays internal. A `notApplicable` declaration in `pages.json` therefore publishes nothing; it keeps a permanent absence out of the report's candidate work.

## Frontmatter

Each published page lives at `<category>/<slug>/README.md` and carries YAML frontmatter that the docs site turns into page metadata. Three fields have length rules, because search engines flag a title tag or meta description that falls outside them:

| Field         | Renders as                                | Length             |
| ------------- | ----------------------------------------- | ------------------ |
| `title`       | Page heading, sidebar label, landing card | Short — it is UI text |
| `seoTitle`    | The browser and search-result title        | 50-60 characters   |
| `description` | The search-result snippet, not shown on the page | 150-160 characters |

`seoTitle` is used exactly as written and must end with ` — Spinifex Docs`. Leave it out and the title falls back to `title` plus that suffix, which is normally too short to satisfy the rule.

Keep the two titles distinct. `title` is navigation text in a narrow sidebar, so stretching it to reach 50 characters damages the layout to satisfy a rule that only applies to the title tag. Put the search-facing wording in `seoTitle`.

A new page is only published once its path is added to `scripts/docs-config.json` in the `mulga-docs` repo. That repo's `scripts/build-docs.js` warns about any field outside its range.
