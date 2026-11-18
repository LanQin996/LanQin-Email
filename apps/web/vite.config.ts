import { readFileSync } from "node:fs"
import path from "node:path"
import react from "@vitejs/plugin-react"
import { defineConfig } from "vite"
import { assertUniqueJSONProperties } from "./scripts/theme-json.mjs"
import { registeredPacks, themePacks } from "./src/themes/catalog"
import { renderThemeCSS } from "./src/themes/contract"

const virtualThemeCSS = "virtual:lanqin-theme-packs.css"
const resolvedThemeCSS = "\0" + virtualThemeCSS

for (const { source } of registeredPacks) {
  assertUniqueJSONProperties(readFileSync(path.resolve(__dirname, source), "utf8"), source)
}

export default defineConfig({
  plugins: [
    react(),
    {
      name: "lanqin-theme-packs",
      resolveId(id) {
        if (id === virtualThemeCSS) return resolvedThemeCSS
      },
      load(id) {
        if (id === resolvedThemeCSS) return renderThemeCSS(themePacks)
      },
    },
  ],
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "./src"),
    },
  },
  server: {
    port: 5173,
    proxy: {
      "/api": "http://localhost:8080",
      "/healthz": "http://localhost:8080",
    },
  },
})
