import assert from "node:assert/strict"
import fs from "node:fs"
import test from "node:test"
import vm from "node:vm"
import { QueryClient, QueryObserver } from "@tanstack/react-query"
import ts from "typescript"

const source = fs.readFileSync(new URL("../src/pages/profile.tsx", import.meta.url), "utf8")
const ast = ts.createSourceFile(
  "profile.tsx",
  source,
  ts.ScriptTarget.Latest,
  true,
  ts.ScriptKind.TSX
)
const profile = ast.statements.find(
  (statement) => ts.isFunctionDeclaration(statement) && statement.name?.text === "ProfilePage"
)
assert.ok(profile?.body)

function findVariable(name) {
  const declaration = profile.body.statements.find(
    (statement) =>
      ts.isVariableStatement(statement) &&
      statement.declarationList.declarations.some(
        (item) => ts.isIdentifier(item.name) && item.name.text === name
      )
  )
  return declaration?.declarationList.declarations.find(
    (item) => ts.isIdentifier(item.name) && item.name.text === name
  )
}

function queryKeyFrom(value) {
  assert.ok(ts.isObjectLiteralExpression(value))
  const property = value.properties.find(
    (item) => ts.isPropertyAssignment(item) && item.name.getText(ast) === "queryKey"
  )
  assert.ok(property && ts.isPropertyAssignment(property))
  return property.initializer
}

function applicationConfig() {
  const mailboxQuery = findVariable("mailboxes")
  assert.ok(mailboxQuery?.initializer && ts.isCallExpression(mailboxQuery.initializer))
  const listQueryKey = JSON.parse(queryKeyFrom(mailboxQuery.initializer.arguments[0]).getText(ast))

  const mutation = findVariable("applyMailbox")
  assert.ok(mutation?.initializer && ts.isCallExpression(mutation.initializer))
  const options = mutation.initializer.arguments[0]
  assert.ok(options && ts.isObjectLiteralExpression(options))
  const onSuccess = options.properties.find(
    (item) =>
      ts.isPropertyAssignment(item) &&
      item.name.getText(ast) === "onSuccess" &&
      ts.isArrowFunction(item.initializer)
  )
  assert.ok(onSuccess && ts.isPropertyAssignment(onSuccess))
  return { listQueryKey, onSuccess: onSuccess.initializer.getText(ast) }
}

test("mailbox application invalidates the owned list and preserves selection and options refresh", () => {
  const { listQueryKey, onSuccess } = applicationConfig()
  const invalidations = []
  let selectedMailboxId = ""
  const onSuccessFn = vm.runInNewContext(`(${onSuccess})`, {
    qc: { invalidateQueries: (query) => invalidations.push(Array.from(query.queryKey)) },
    setMailboxId: (id) => (selectedMailboxId = id),
    toast: () => {},
  })
  onSuccessFn({ id: "mailbox-created" })

  assert.ok(invalidations.some((key) => JSON.stringify(key) === JSON.stringify(listQueryKey)))
  assert.ok(invalidations.some((key) => JSON.stringify(key) === '["mailbox-apply-options"]'))
  assert.equal(selectedMailboxId, "mailbox-created")
})

test("successful application refreshes an active owned mailbox list without a page reload", async () => {
  const { listQueryKey, onSuccess } = applicationConfig()
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: Infinity } },
  })
  const original = { id: "mailbox-original", address: "original@example.test" }
  const created = { id: "mailbox-created", address: "created@example.test" }
  client.setQueryData(listQueryKey, { items: [original] })
  const observer = new QueryObserver(client, {
    queryKey: listQueryKey,
    queryFn: async () => ({ items: [original, created] }),
    staleTime: Infinity,
  })
  const unsubscribe = observer.subscribe(() => {})
  try {
    assert.deepEqual(observer.getCurrentResult().data.items, [original])
    const refreshes = []
    const onSuccessFn = vm.runInNewContext(`(${onSuccess})`, {
      qc: {
        invalidateQueries: (query) => {
          const refresh = client.invalidateQueries(query)
          refreshes.push(refresh)
          return refresh
        },
      },
      setMailboxId: () => {},
      toast: () => {},
    })
    onSuccessFn(created)
    await Promise.all(refreshes)
    assert.deepEqual(observer.getCurrentResult().data.items, [original, created])
  } finally {
    unsubscribe()
    client.clear()
  }
})
