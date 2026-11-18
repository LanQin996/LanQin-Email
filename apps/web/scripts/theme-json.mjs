import ts from "typescript"

export function assertUniqueJSONProperties(text, source) {
  const file = ts.parseJsonText(source, text)
  function visit(node, scope) {
    if (ts.isObjectLiteralExpression(node)) {
      const seen = new Set()
      for (const property of node.properties) {
        if (!ts.isPropertyAssignment(property) || !ts.isStringLiteral(property.name)) continue
        const key = property.name.text
        const path = scope ? `${scope}.${key}` : key
        if (seen.has(key)) throw new Error(`${source}: duplicate property ${path}`)
        seen.add(key)
        visit(property.initializer, path)
      }
    } else {
      ts.forEachChild(node, (child) => visit(child, scope))
    }
  }
  visit(file, "")
}
