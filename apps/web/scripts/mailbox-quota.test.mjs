import assert from "node:assert/strict"
import fs from "node:fs"
import path from "node:path"
import test from "node:test"
import vm from "node:vm"
import React from "react"
import { renderToStaticMarkup } from "react-dom/server"
import ts from "typescript"
import { sourceLoader, webRoot } from "./i18n-test-loader.mjs"

function sourceFile(filename) {
  return ts.createSourceFile(
    filename,
    fs.readFileSync(path.join(webRoot, filename), "utf8"),
    ts.ScriptTarget.Latest,
    true,
    ts.ScriptKind.TSX
  )
}

function findNode(root, predicate) {
  if (predicate(root)) return root
  return ts.forEachChild(root, (child) => findNode(child, predicate))
}

test("profile quota uses current ownership, not creation history or visible mailbox lists", () => {
  const source = sourceFile("src/pages/profile.tsx")
  const quota = findNode(
    source,
    (node) => ts.isJsxAttribute(node) && node.name.text === "mailboxQuota"
  )
  assert.ok(quota?.initializer && ts.isJsxExpression(quota.initializer))
  const getQuota = new Function("user", `return (${quota.initializer.expression.getText(source)})`)
  assert.deepEqual(
    getQuota({
      mailboxCount: 1,
      mailboxesCreatedTotal: 17,
      mailboxQuotaBonus: 2,
      limits: { maxMailboxes: 3 },
    }),
    { used: 1, limit: 5 }
  )
  assert.deepEqual(
    getQuota({
      mailboxCount: 0,
      mailboxesCreatedTotal: 17,
      mailboxQuotaBonus: 2,
      limits: { maxMailboxes: 0 },
    }),
    { used: 0, limit: 0 }
  )
  assert.equal(getQuota(undefined), undefined)
})

// Exercise the real dialog with mocked hooks and layout primitives, not a browser.
test("admin details separate current usage from history and rerender in all languages", () => {
  const source = sourceFile("src/pages/admin.tsx")
  const dialog = source.statements.find(
    (node) => ts.isFunctionDeclaration(node) && node.name?.text === "UserDetailsDialog"
  )
  assert.ok(dialog)
  const compiled = ts.transpileModule(
    `${dialog.getText(source)}; globalThis.renderDialog = UserDetailsDialog`,
    {
      compilerOptions: {
        module: ts.ModuleKind.CommonJS,
        target: ts.ScriptTarget.ES2020,
        jsx: ts.JsxEmit.React,
      },
    }
  ).outputText
  const { uiText } = sourceLoader()("src/lib/language.tsx")
  let language = "zh-CN"
  const context = {
    React,
    useUiLanguage: () => {},
    uiText: (key, params) => uiText(key, params, language),
    formatDateTime: () => "2026-10-28",
    limitText: (limit) => String(limit),
    groupPermissionCatalog: () => [],
    permissionMessages: {},
    localizePermissionInfo: (value) => value,
  }
  for (const name of [
    "Dialog",
    "DialogContent",
    "DialogHeader",
    "DialogTitle",
    "DialogFooter",
    "Badge",
    "Button",
    "Separator",
    "RoleBadge",
    "UserPermissionGroupsCell",
  ])
    context[name] = ({ children }) => React.createElement("div", null, children)
  vm.createContext(context)
  vm.runInContext(compiled, context)
  const user = {
    id: "synthetic-user",
    email: "synthetic@example.test",
    displayName: "Synthetic",
    createdAt: "2026-10-28T00:00:00Z",
    permissions: [],
    mailboxCount: 1,
    mailboxesCreatedTotal: 17,
    mailboxQuotaBonus: 0,
    mailboxes: ["synthetic@quota.test"],
    limits: { maxMailboxes: 3, maxMailboxesPerDay: 2 },
  }
  for (const code of ["zh-CN", "zh-TW", "en"]) {
    language = code
    for (const count of [1, 0]) {
      const html = renderToStaticMarkup(
        context.renderDialog({
          user: { ...user, mailboxCount: count, mailboxes: count ? user.mailboxes : [] },
          onClose: () => {},
          onRestoreFocus: () => {},
        })
      )
      assert.ok(
        html.includes(
          `${uiText("已用邮箱额度", undefined, code)}</dt><dd class="shrink-0 font-medium">${count}</dd>`
        )
      )
      assert.ok(
        html.includes(
          `${uiText("累计创建邮箱", undefined, code)}</dt><dd class="shrink-0 font-medium">17</dd>`
        )
      )
      assert.ok(html.includes(uiText("按当前拥有邮箱数计算", undefined, code)))
      assert.ok(html.includes(uiText("删除邮箱会释放总额度", undefined, code)))
      assert.ok(!html.includes("does not restore"))
    }
  }
})

test("mailbox application refreshes current quota usage", () => {
  const source = sourceFile("src/pages/profile.tsx")
  const mutation = findNode(
    source,
    (node) =>
      ts.isVariableDeclaration(node) &&
      ts.isIdentifier(node.name) &&
      node.name.text === "applyMailbox"
  )
  assert.ok(mutation?.initializer && ts.isCallExpression(mutation.initializer))
  const onSuccess = mutation.initializer.arguments[0].properties.find(
    (node) => ts.isPropertyAssignment(node) && node.name.getText(source) === "onSuccess"
  )
  assert.ok(onSuccess)
  const invalidated = []
  const callback = new Function(
    "qc",
    "setMailboxId",
    "toast",
    `return ${onSuccess.initializer.getText(source)}`
  )(
    { invalidateQueries: ({ queryKey }) => invalidated.push(queryKey) },
    () => {},
    () => {}
  )
  callback({ id: "synthetic-mailbox" })
  assert.ok(invalidated.some((key) => key.length === 1 && key[0] === "me"))
})

test("quota rejection remains a safe translatable error across language changes", () => {
  const load = sourceLoader({
    window: {
      localStorage: { getItem: () => "zh-CN", setItem: () => {} },
      navigator: { language: "zh-CN" },
      dispatchEvent: () => {},
    },
    CustomEvent: class {
      constructor(type) {
        this.type = type
      }
    },
  })
  const { ApiError } = load("src/lib/ui-errors.ts")
  const { uiText, setStoredLanguage } = load("src/lib/language.tsx")
  const key = "邮箱数量已达上限，请先删除不再使用的邮箱或联系管理员"
  const error = new ApiError(409, key)
  assert.equal(error.message, key)
  const messages = new Set()
  for (const code of ["zh-CN", "zh-TW", "en"]) {
    setStoredLanguage(code)
    messages.add(uiText(error.message))
  }
  assert.equal(messages.size, 3)
})
