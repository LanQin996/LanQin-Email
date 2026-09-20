import fs from "node:fs"
import path from "node:path"
import ts from "typescript"
import { sourceLoader, webRoot } from "./i18n-test-loader.mjs"

const { exactTranslations } = sourceLoader()("src/lib/language.tsx")
const failures = []
const placeholders = (text) => [...text.matchAll(/\{(\d+)\}/g)].map((m) => m[1]).sort().join(",")
for (const [key, translations] of Object.entries(exactTranslations)) {
  for (const language of ["zh-TW", "en", ...(translations.enOne ? ["enOne"] : [])]) {
    const text = translations[language]
    if (!text || placeholders(key) !== placeholders(text)) {
      failures.push(`Dictionary ${JSON.stringify(key)}: missing ${language} or mismatched parameters`)
    }
  }
}

// Protocols, product names, sample addresses and punctuation are not interface prose.
const literalExemptions = new Set([
  "LanQin Email", "DNS", "SMTP", "HTML", "SSL/TLS", "STARTTLS", "Message-ID",
  "Message-ID：", "Telegram", "GitHub", "Gmail OAuth2", "Microsoft 365 / Outlook OAuth2",
  "Linux.do SSO", "Turnstile", "API Token", "● IMAP", "● POP3", "● SMTP", "MB",
  "TTL:", "s", "mail.example.com", "https://mail.example.com", "127.0.0.1",
  "test@example.com", "user@example.com", "example.com", "alice", "Alice",
  "alice@example.com", "cc@example.com", "bcc@example.com", "your-name",
  "name@example.com", "imap.example.com", "billing-system",
])
const attributes = new Set(["title", "placeholder", "aria-label", "label", "text", "closeLabel"])
const messageProperties = new Set(["title", "description", "label", "text", "confirmText", "cancelText"])
// This is a server response discriminator, never rendered directly.
const nonUiLiterals = new Map([["src/lib/diagnostic-messages.ts", new Set(["记录有效"])]])
const hasHan = (text) => /[\u3400-\u9fff]/.test(text)
function exempt(text) {
  return literalExemptions.has(text) || /^[\d\s@+/:：·().%-]*$/.test(text)
}
function walk(dir) {
  return fs.readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const filename = path.join(dir, entry.name)
    return entry.isDirectory() ? walk(filename) : /\.(ts|tsx)$/.test(entry.name) ? [filename] : []
  })
}
for (const filename of walk(path.join(webRoot, "src"))) {
  const relative = path.relative(webRoot, filename).replaceAll("\\", "/")
  if (relative === "src/lib/language.tsx") continue
  const source = fs.readFileSync(filename, "utf8")
  const tree = ts.createSourceFile(filename, source, ts.ScriptTarget.Latest, true)
  function report(node, message) {
    failures.push(`${relative}:${tree.getLineAndCharacterOfPosition(node.getStart(tree)).line + 1}: ${message}`)
  }
  function visit(node) {
    if (ts.isJsxText(node) && node.text.trim() && !exempt(node.text.trim())) {
      report(node, `Use explicit translation for JSX text ${JSON.stringify(node.text.trim())}`)
    }
    if (ts.isJsxAttribute(node) && attributes.has(node.name.getText(tree)) &&
        node.initializer && ts.isStringLiteral(node.initializer) &&
        !exempt(node.initializer.text)) {
      // UiLabel is itself a subscribed translation boundary.
      const tag = node.parent.parent.tagName.getText(tree)
      if (!(tag === "UiLabel" && node.name.getText(tree) === "text")) {
        report(node, `Use explicit translation for ${node.name.getText(tree)}`)
      }
    }
    if (ts.isCallExpression(node) && ["uiText", "uiMessage", "translateUiText", "t"].includes(node.expression.getText(tree))) {
      const key = node.arguments[0]
      if (key && ts.isStringLiteral(key) && key.text && !Object.hasOwn(exactTranslations, key.text)) {
        report(key, `Missing dictionary entry ${JSON.stringify(key.text)}`)
      }
    }
    if (ts.isPropertyAssignment(node) && messageProperties.has(node.name.getText(tree)) &&
        ts.isStringLiteral(node.initializer) && node.initializer.text &&
        !exempt(node.initializer.text) && !Object.hasOwn(exactTranslations, node.initializer.text)) {
      report(node, `Uncatalogued message/configuration text ${JSON.stringify(node.initializer.text)}`)
    }
    // Config labels, deferred toast text and error messages must also have translations.
    if (ts.isStringLiteral(node) && hasHan(node.text) && !Object.hasOwn(exactTranslations, node.text)) {
      const isPropertyKey = ts.isPropertyAssignment(node.parent) && node.parent.name === node
      const isContentCode = ["src/components/mail-content.ts", "src/components/mail-schedule-utils.ts"].includes(relative)
      const isLanguageName = relative === "src/pages/mail.tsx" && ["微软雅黑"].includes(node.text)
      if (!isPropertyKey && !isContentCode && !isLanguageName &&
          !nonUiLiterals.get(relative)?.has(node.text) && node.text !== "请求超时") {
        report(node, `Uncatalogued UI/configuration text ${JSON.stringify(node.text)}`)
      }
    }
    ts.forEachChild(node, visit)
  }
  visit(tree)
}
if (failures.length) {
  console.error(failures.join("\n"))
  process.exitCode = 1
} else {
  console.log(`i18n checks passed: ${Object.keys(exactTranslations).length} complete translation entries`)
}
