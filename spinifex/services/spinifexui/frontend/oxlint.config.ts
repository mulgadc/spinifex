import { defineConfig } from "oxlint"
import core from "ultracite/oxlint/core"
import react from "ultracite/oxlint/react"
import tanstack from "ultracite/oxlint/tanstack"
import vitest from "ultracite/oxlint/vitest"

export default defineConfig({
  extends: [core, react, vitest, tanstack],
  jsPlugins: [
    "@tanstack/eslint-plugin-query",
    "@tanstack/eslint-plugin-router",
    "eslint-plugin-react-you-might-not-need-an-effect",
  ],
  options: {
    typeAware: true,
    typeCheck: true,
  },
  rules: {
    "@tanstack/query/exhaustive-deps": "error",
    "@tanstack/query/infinite-query-property-order": "error",
    "@tanstack/query/mutation-property-order": "error",
    "@tanstack/query/no-rest-destructuring": "error",
    "@tanstack/query/no-unstable-deps": "error",
    "@tanstack/query/no-void-query-fn": "error",
    "@tanstack/query/stable-query-client": "error",
    "@tanstack/router/create-route-property-order": "error",
    "@tanstack/router/route-param-names": "error",
    "eslint/complexity": "off",
    "eslint/func-style": [
      "error",
      "declaration",
      {
        allowArrowFunctions: true,
      },
    ],
    "eslint/no-await-in-loop": "off",
    "eslint/no-console": "error",
    "eslint/no-plusplus": ["error", { allowForLoopAfterthoughts: true }],
    "eslint/no-use-before-define": "off",
    "eslint/require-unicode-regexp": "off",
    "eslint/sort-keys": "off",
    "import/consistent-type-specifier-style": "off",
    "jsx-a11y/prefer-tag-over-role": "off",
    "react/function-component-definition": "off",
    "react/jsx-handler-names": "off",
    "react-you-might-not-need-an-effect/no-adjust-state-on-prop-change":
      "error",
    "react-you-might-not-need-an-effect/no-chain-state-updates": "error",
    "react-you-might-not-need-an-effect/no-derived-state": "error",
    "react-you-might-not-need-an-effect/no-event-handler": "error",
    "react-you-might-not-need-an-effect/no-external-store-subscription":
      "error",
    "react-you-might-not-need-an-effect/no-initialize-state": "error",
    "react-you-might-not-need-an-effect/no-pass-live-state-to-parent": "error",
    "react-you-might-not-need-an-effect/no-reset-all-state-on-prop-change":
      "error",
    "typescript/no-confusing-void-expression": "off",
    "typescript/no-floating-promises": [
      "error",
      {
        allowForKnownSafeCalls: [
          {
            from: "package",
            name: "UseNavigateResult",
            package: "@tanstack/router-core",
          },
        ],
      },
    ],
    "typescript/no-misused-promises": "off",
    "typescript/only-throw-error": [
      "error",
      {
        allow: [
          {
            from: "package",
            name: "Redirect",
            package: "@tanstack/router-core",
          },
        ],
      },
    ],
    "typescript/strict-boolean-expressions": "off",
    "typescript/strict-void-return": "off",
    "unicorn/filename-case": [
      "error",
      { cases: { kebabCase: true, camelCase: true } },
    ],
    "unicorn/prefer-single-call": "off",
  },
  overrides: [
    {
      files: [
        "**/*.{test,spec}.{ts,tsx,js,jsx}",
        "**/__tests__/**/*.{ts,tsx,js,jsx}",
      ],
      plugins: ["vitest"],
      rules: {
        "eslint/prefer-destructuring": "off",
        "eslint/require-await": "off",
        "import/first": "off",
        "typescript/consistent-type-imports": "off",
        "typescript/no-non-null-assertion": "off",
        "typescript/no-unsafe-argument": "off",
        "typescript/no-unsafe-assignment": "off",
        "typescript/no-unsafe-member-access": "off",
        "typescript/no-unsafe-type-assertion": "off",
        "unicorn/no-useless-undefined": "off",
        "vitest/max-expects": "off",
        "vitest/prefer-called-once": "off",
        "vitest/prefer-called-with": "off",
        "vitest/prefer-describe-function-title": "off",
        "vitest/prefer-expect-resolves": "off",
        "vitest/prefer-import-in-mock": "off",
        "vitest/require-mock-type-parameters": "off",
      },
    },
  ],
  ignorePatterns: core.ignorePatterns,
})
