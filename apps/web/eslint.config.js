import eslint from "@eslint/js";
import typescriptEslint from "@typescript-eslint/eslint-plugin";
import reactHooks from "eslint-plugin-react-hooks";
import reactRefresh from "eslint-plugin-react-refresh";
import globals from "globals";

const sourceFiles = ["**/*.{ts,tsx}"];
const forSourceFiles = (config) => ({ ...config, files: sourceFiles });

export default [
  {
    ignores: [
      "dist",
      "node_modules",
      ".next",
      "build",
      "coverage",
      "*.config.js",
      "*.config.ts",
      "vite.config.ts",
      "*.d.ts",
    ],
  },
  forSourceFiles(eslint.configs.recommended),
  ...typescriptEslint.configs["flat/recommended"].map(forSourceFiles),
  forSourceFiles(reactHooks.configs["recommended-latest"]),
  forSourceFiles(reactRefresh.configs.vite),
  {
    files: sourceFiles,
    languageOptions: {
      ecmaVersion: 2020,
      globals: globals.browser,
    },
    rules: {
      "react-refresh/only-export-components": [
        "warn",
        { allowConstantExport: true },
      ],
      "@typescript-eslint/no-unused-vars": [
        "warn",
        {
          argsIgnorePattern: "^_",
          varsIgnorePattern: "^_",
        },
      ],
      "@typescript-eslint/no-explicit-any": "warn",
    },
  },
  {
    files: ["src/components/ui/badge.tsx"],
    rules: {
      "react-refresh/only-export-components": [
        "warn",
        { allowConstantExport: true, allowExportNames: ["badgeVariants"] },
      ],
    },
  },
  {
    files: ["src/components/ui/button.tsx"],
    rules: {
      "react-refresh/only-export-components": [
        "warn",
        { allowConstantExport: true, allowExportNames: ["buttonVariants"] },
      ],
    },
  },
  {
    files: ["src/components/ui/sidebar.tsx"],
    rules: {
      "react-refresh/only-export-components": [
        "warn",
        { allowConstantExport: true, allowExportNames: ["useSidebar"] },
      ],
    },
  },
  {
    files: ["src/lib/language.tsx"],
    rules: {
      "react-refresh/only-export-components": [
        "warn",
        {
          allowConstantExport: true,
          allowExportNames: [
            "languageOptions",
            "getInitialLanguage",
            "setStoredLanguage",
            "useLanguage",
            "translateUiText",
          ],
        },
      ],
    },
  },
];
