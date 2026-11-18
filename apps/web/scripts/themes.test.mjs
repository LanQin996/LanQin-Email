import assert from "node:assert/strict"
import { readFileSync } from "node:fs"
import test from "node:test"
import ts from "typescript"
import postcss from "postcss"
import { assertUniqueJSONProperties } from "./theme-json.mjs"

function transpileThemeModule(name, transform = (source) => source) {
  const source = readFileSync(new URL(`../src/themes/${name}.ts`, import.meta.url), "utf8")
  const { outputText } = ts.transpileModule(transform(source), {
    compilerOptions: { target: ts.ScriptTarget.ES2020, module: ts.ModuleKind.ESNext },
  })
  return "data:text/javascript;base64," + Buffer.from(outputText).toString("base64")
}
const contractModule = transpileThemeModule("contract")
const { DEFAULT_THEME_ID, readThemePack, renderThemeCSS, resolveThemeId, validateThemeIds } =
  await import(contractModule)
const { normalizeThemePackage, runtimeThemeCSS } = await import(
  transpileThemeModule("runtime-package", (source) => source.replace("./contract", contractModule))
)

const paperText = readFileSync(new URL("../src/themes/paper/theme.json", import.meta.url), "utf8")
const paper = JSON.parse(paperText)

test("valid light/dark pack emits only theme-specific selectors and falls back to Classic", () => {
  assert.doesNotThrow(() => assertUniqueJSONProperties(paperText, "paper"))
  const sample = readThemePack(paper, "paper")
  const classic = { version: 1, id: DEFAULT_THEME_ID, name: "经典主题" }
  const catalog = [classic, sample]
  assert.doesNotThrow(() => validateThemeIds(catalog))
  assert.equal(resolveThemeId("paper", catalog), "paper")
  for (const id of ["not-installed", null, undefined]) {
    assert.equal(resolveThemeId(id, catalog), "classic")
  }
  const css = postcss.parse(renderThemeCSS([sample]))
  assert.deepEqual(
    css.nodes.map((node) => node.selector),
    ['html:not(.dark)[data-lanqin-theme="paper"]', 'html.dark[data-lanqin-theme="paper"]']
  )
  assert.equal(css.nodes[0].nodes.find(({ prop }) => prop === "--background").value, "38 27% 96%")
  assert.equal(css.nodes[1].nodes.find(({ prop }) => prop === "--background").value, "29 14% 12%")
})

test("duplicate IDs and duplicate JSON keys fail", () => {
  const sample = readThemePack(paper, "paper")
  assert.throws(() => validateThemeIds([sample, sample]), /Duplicate theme ID: paper/)
  assert.throws(
    () =>
      assertUniqueJSONProperties(
        '{"light":{"background":"first","backgrou\\u006ed":"last"}}',
        "paper"
      ),
    /paper: duplicate property light\.background/
  )
})

test("rejects unknown keys, invalid metadata and malformed or unsafe values", () => {
  for (const value of [
    { ...paper, version: 2 },
    { ...paper, id: "classic" },
    { ...paper, id: 'foo"]' },
    { ...paper, id: "paper\n" },
    { ...paper, name: "" },
    { ...paper, css: "" },
    { ...paper, dark: null },
    { ...paper, light: { invalid: "0 0% 0%" } },
    { ...paper, light: { background: "0 101% 50%" } },
    { ...paper, light: { background: "361 10% 50%" } },
    { ...paper, light: { background: "0 10% 50%\n" } },
    { ...paper, light: { background: "rgb(255 0 0)" } },
    { ...paper, light: { background: "var(--foreground)" } },
    { ...paper, light: { background: "url(https://example.test)" } },
    { ...paper, light: { radius: "calc(1rem + 1rem)" } },
    { ...paper, light: { radius: "3rem" } },
    { ...paper, light: { radius: 1 } },
  ]) {
    assert.throws(() => readThemePack(value, "paper"), String(JSON.stringify(value)))
  }
})

function luminance(color) {
  const [h, s, l] = color.split(" ").map(Number.parseFloat)
  const f = (n) => {
    const k = (n + h / 30) % 12
    const channel =
      l / 100 - (s / 100) * Math.min(l / 100, 1 - l / 100) * Math.max(-1, Math.min(k - 3, 9 - k, 1))
    return channel <= 0.04045 ? channel / 12.92 : ((channel + 0.055) / 1.055) ** 2.4
  }
  return 0.2126 * f(0) + 0.7152 * f(8) + 0.0722 * f(4)
}

test("Paper text pairs meet 4.5:1 contrast on key surfaces", () => {
  const css = postcss.parse(
    readFileSync(new URL("../src/themes/classic/styles.css", import.meta.url), "utf8")
  )
  const pairs = [
    ["background", "foreground"],
    ["card", "card-foreground"],
    ["popover", "popover-foreground"],
    ["primary", "primary-foreground"],
    ["secondary", "secondary-foreground"],
    ["muted", "muted-foreground"],
    ["accent", "accent-foreground"],
    ["sidebar-background", "sidebar-foreground"],
    ["sidebar-accent", "sidebar-accent-foreground"],
  ]
  const classicLight = Object.fromEntries(
    css.nodes[0].nodes.map(({ prop, value }) => [prop.slice(2), value])
  )
  const classicDark = {
    ...classicLight,
    ...Object.fromEntries(css.nodes[1].nodes.map(({ prop, value }) => [prop.slice(2), value])),
  }
  const palettes = {
    "Paper light": { ...classicLight, ...paper.light },
    "Paper dark": { ...classicDark, ...paper.dark },
  }
  for (const [label, tokens] of Object.entries(palettes)) {
    for (const [surface, text] of pairs) {
      const a = luminance(tokens[surface])
      const b = luminance(tokens[text])
      const ratio = (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05)
      assert.ok(ratio >= 4.5, `${label} ${surface}/${text} contrast is ${ratio.toFixed(2)}:1`)
    }
  }
})

test("layout pack manifests validate independently for each area", async () => {
  const source = readFileSync(new URL("../src/themes/layouts/manifest.ts", import.meta.url), "utf8")
  const { outputText: js } = ts.transpileModule(source, {
    compilerOptions: { target: ts.ScriptTarget.ES2020, module: ts.ModuleKind.ESNext },
  })
  const { isLayoutManifest, resolveLayoutFrom } = await import(
    "data:text/javascript;base64," + Buffer.from(js).toString("base64")
  )
  const readPack = (id) => {
    const path = new URL(`../src/themes/layouts/packs/${id}/theme.json`, import.meta.url)
    const raw = readFileSync(path, "utf8")
    assertUniqueJSONProperties(raw, path.pathname)
    const value = JSON.parse(raw)
    assert.equal(value.id, id)
    assert.ok(isLayoutManifest(value))
    return value
  }
  const classic = readPack("classic")
  const workbench = readPack("workbench")
  const fixture = { ...workbench, id: "mail-only", surfaces: ["front"] }
  assert.ok(isLayoutManifest(fixture))
  assert.ok(!isLayoutManifest({ ...workbench, apiVersion: 2 }))
  assert.ok(!isLayoutManifest({ ...workbench, id: " invalid\n" }))
  assert.ok(!isLayoutManifest({ ...workbench, surfaces: ["front", "front"] }))
  const layouts = [classic, workbench, fixture]
  assert.equal(resolveLayoutFrom(layouts, "workbench", "admin"), "workbench")
  assert.equal(resolveLayoutFrom(layouts, "mail-only", "front"), "mail-only")
  assert.equal(resolveLayoutFrom(layouts, "mail-only", "admin"), "classic")
  assert.equal(resolveLayoutFrom(layouts, "missing", "front"), "classic")
})


test("runtime package CSS uses only scoped allowlisted tokens from the selected appearance", () => {
  const front = normalizeThemePackage({
    layout: "workbench",
    light: { background: "40 26% 96%", radius: "0.65rem" },
    dark: { background: "28 17% 12%" },
  })
  const admin = normalizeThemePackage({
    layout: "classic",
    light: { background: "45 15% 97%" },
    dark: {},
  })
  assert.equal(front.layout, "workbench")
  assert.equal(admin.layout, "classic")
  const frontCSS = postcss.parse(runtimeThemeCSS(front)).nodes
  const adminCSS = postcss.parse(runtimeThemeCSS(admin)).nodes
  assert.deepEqual(
    frontCSS.map((rule) => rule.selector),
    ["html:root[data-lanqin-theme]:not(.dark)", "html:root[data-lanqin-theme].dark"]
  )
  assert.equal(frontCSS[0].nodes.find(({ prop }) => prop === "--background").value, "40 26% 96%")
  assert.equal(frontCSS[1].nodes.find(({ prop }) => prop === "--background").value, "28 17% 12%")
  assert.equal(adminCSS[0].nodes.find(({ prop }) => prop === "--background").value, "45 15% 97%")
  assert.equal(adminCSS[0].nodes.some(({ prop }) => prop === "--radius"), false)
  assert.equal(normalizeThemePackage({ ...front, layout: "external" }), undefined)
  assert.equal(normalizeThemePackage({ ...front, light: null }), undefined)
  assert.equal(normalizeThemePackage({ ...front, light: { radius: "3rem" } }), undefined)
  assert.equal(normalizeThemePackage({ ...front, dark: { unknown: "0 0% 0%" } }), undefined)
})
