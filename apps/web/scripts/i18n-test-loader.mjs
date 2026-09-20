import fs from "node:fs"
import path from "node:path"
import vm from "node:vm"
import { createRequire } from "node:module"
import { fileURLToPath } from "node:url"
import ts from "typescript"

export const webRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..")
const requireDependency = createRequire(path.join(webRoot, "package.json"))

// Compile the actual source in memory, without generating or altering source files.
export function sourceLoader(globals = {}) {
  const cache = new Map()
  const context = vm.createContext({ console, Intl, Date, ...globals })
  function load(filename) {
    let resolved = filename.startsWith("@/")
      ? path.join(webRoot, "src", filename.slice(2))
      : path.resolve(webRoot, filename)
    if (!path.extname(resolved)) {
      resolved = [".ts", ".tsx", ".json"].map((ext) => resolved + ext).find(fs.existsSync)
    }
    if (!resolved) throw new Error(`Module not found: ${filename}`)
    if (cache.has(resolved)) return cache.get(resolved).exports
    const module = { exports: {} }
    cache.set(resolved, module)
    if (resolved.endsWith(".json")) {
      module.exports = JSON.parse(fs.readFileSync(resolved, "utf8"))
      return module.exports
    }
    const compiled = ts.transpileModule(fs.readFileSync(resolved, "utf8"), {
      compilerOptions: {
        module: ts.ModuleKind.CommonJS,
        target: ts.ScriptTarget.ES2020,
        jsx: ts.JsxEmit.ReactJSX,
        esModuleInterop: true,
      },
    }).outputText
    const localRequire = (name) =>
      name.startsWith("@/")
        ? load(name)
        : name.startsWith(".")
          ? load(path.resolve(path.dirname(resolved), name))
          : requireDependency(name)
    vm.runInContext(`(function(require,module,exports){${compiled}\n})`, context, {
      filename: resolved,
    })(localRequire, module, module.exports)
    return module.exports
  }
  return load
}
