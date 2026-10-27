import assert from "node:assert/strict"
import fs from "node:fs"
import path from "node:path"
import test from "node:test"
import vm from "node:vm"
import * as React from "react"
import ts from "typescript"
import { sourceLoader, webRoot } from "./i18n-test-loader.mjs"

// Exercise the real dialog function without introducing a test-only UI dependency.
// Hooks and UI primitives are mocked; this is not a browser/end-to-end test.
const source = fs.readFileSync(path.join(webRoot, "src/pages/admin.tsx"), "utf8")
const ast = ts.createSourceFile(
  "admin.tsx",
  source,
  ts.ScriptTarget.Latest,
  true,
  ts.ScriptKind.TSX
)
const dialog = ast.statements.find(
  (node) => ts.isFunctionDeclaration(node) && node.name?.text === "CreateMailboxDialog"
)
assert.ok(dialog)
const compiled = ts.transpileModule(
  `${dialog.getText(ast)}; globalThis.renderDialog = CreateMailboxDialog`,
  {
    compilerOptions: {
      module: ts.ModuleKind.CommonJS,
      target: ts.ScriptTarget.ES2020,
      jsx: ts.JsxEmit.React,
    },
  }
).outputText

function nodesIn(value, nodes = []) {
  if (Array.isArray(value)) value.forEach((child) => nodesIn(child, nodes))
  else if (value && typeof value === "object") {
    nodes.push(value)
    nodesIn(value.props?.children, nodes)
  }
  return nodes
}

function setup() {
  const load = sourceLoader()
  const language = load("src/lib/language.tsx")
  let cursor = 0
  const state = [true, "dom_test", "usr_active"]
  const calls = []
  const effects = []
  const context = {
    React: {
      ...React,
      useState: () => {
        const index = cursor++
        return [state[index], (value) => (state[index] = value)]
      },
      useEffect: (effect) => effects.push(effect),
    },
    useUiLanguage: () => {},
    useQueryClient: () => ({}),
    useToast: () => ({ toast: () => {} }),
    useMutation: (config) => {
      context.mutation = config
      return { isPending: context.pending, mutate: (form) => config.mutationFn(form) }
    },
    uiText: (key) => language.uiText(key, undefined, context.language),
    api: { createMailbox: (payload) => calls.push(JSON.parse(JSON.stringify(payload))) },
    invalidateAdmin: () => {},
    errorMessage: () => "",
    FormData: class {
      get(name) {
        return { localPart: "synthetic", displayName: "Synthetic", quotaMb: "1024" }[name] ?? null
      }
    },
  }
  for (const name of [
    "Dialog",
    "DialogTrigger",
    "Button",
    "Plus",
    "DialogContent",
    "DialogHeader",
    "DialogTitle",
    "DomainSelect",
    "Field",
    "SelectField",
    "Empty",
    "DialogFooter",
  ])
    context[name] = name
  vm.createContext(context)
  vm.runInContext(compiled, context)
  return {
    calls,
    state,
    context,
    effects,
    render(users, code = "zh-CN", pending = false) {
      context.language = code
      context.pending = pending
      cursor = 0
      effects.length = 0
      return nodesIn(context.renderDialog({ domains: [{ id: "dom_test" }], users }))
    },
  }
}

const active = { id: "usr_active", email: "owner@example.test", disabled: false }
const disabled = { id: "usr_disabled", email: "disabled@example.test", disabled: true }

test("add mailbox only selects existing users and submits no user-creation fields in every language", () => {
  const harness = setup()
  for (const language of ["zh-CN", "zh-TW", "en"]) {
    const nodes = harness.render([disabled, active], language)
    assert.equal(nodes.filter((node) => node.type === "SelectField").length, 1)
    const select = nodes.find((node) => node.type === "SelectField")
    assert.equal(select.props.items.length, 1)
    assert.equal(select.props.items[0][0], active.id)
    assert.equal(
      nodes.some((node) => ["password", "ownerEmail"].includes(node.props?.name)),
      false
    )
    assert.equal(
      nodes.find((node) => node.type === "DialogContent").props["aria-describedby"],
      undefined
    )
    nodes.find((node) => node.type === "form").props.onSubmit({ preventDefault() {} })
    assert.deepEqual(harness.calls.at(-1), {
      domainId: "dom_test",
      localPart: "synthetic",
      displayName: "Synthetic",
      quotaMb: 1024,
      userId: active.id,
    })
  }
})

test("missing, disabled and stale users cannot submit or silently create a user", () => {
  const harness = setup()
  for (const users of [[], [disabled]]) {
    const nodes = harness.render(users)
    assert.equal(nodes.find((node) => node.type === "SelectField").props.disabled, true)
    assert.equal(nodes.find((node) => node.type === "Empty").props.text.length > 0, true)
    assert.equal(
      nodes.find((node) => node.type === "Button" && "disabled" in node.props).props.disabled,
      true
    )
    nodes.find((node) => node.type === "form").props.onSubmit({ preventDefault() {} })
  }
  assert.equal(harness.calls.length, 0)
  harness.state[2] = disabled.id
  harness.render([disabled, active])
  harness.effects.forEach((effect) => effect())
  assert.equal(harness.state[2], active.id)
  const nodes = harness.render([active], "en", true)
  assert.equal(
    nodes.find((node) => node.type === "Button" && "disabled" in node.props).props.disabled,
    true
  )
})
