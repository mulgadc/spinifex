import { defineConfig } from "oxfmt"

export default defineConfig({
  printWidth: 80,
  semi: false,
  sortPackageJson: false,
  sortImports: {
    newlinesBetween: true,
  },
  sortTailwindcss: {
    stylesheet: "./src/styles.css",
    functions: ["clsx", "cn", "cva"],
  },
  ignorePatterns: ["dist/**", "src/routeTree.gen.ts"],
})
