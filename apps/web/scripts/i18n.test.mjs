import assert from "node:assert/strict"
import test from "node:test"
import { sourceLoader } from "./i18n-test-loader.mjs"

function setup(language = "en", storageFails = false) {
  let stored = language
  const events = []
  const load = sourceLoader({
    window: {
      navigator: { language: "en-US" },
      localStorage: {
        getItem: () => {
          if (storageFails) throw new Error("blocked")
          return stored
        },
        setItem: (_key, value) => {
          if (storageFails) throw new Error("blocked")
          stored = value
        },
      },
      dispatchEvent: (event) => events.push(event),
    },
    CustomEvent: class { constructor(type, options) { this.type = type; this.detail = options.detail } },
  })
  return { load, events, ...load("src/lib/language.tsx") }
}

test("all dictionaries preserve interpolation parameters and translate all three languages", () => {
  const { exactTranslations, uiText } = setup()
  for (const [key, row] of Object.entries(exactTranslations)) {
    assert.equal(uiText(key, undefined, "zh-CN"), key)
    assert.equal(typeof row.en, "string")
    assert.equal(typeof row["zh-TW"], "string")
  }
})

test("deferred confirmations, multiline warnings and toast messages follow language changes", () => {
  const { uiMessage, uiText, setStoredLanguage } = setup()
  const notice = uiMessage("将删除 {0} 及其关联数据。", ["收件箱"])
  assert.equal(uiText(notice), "This will delete 收件箱 and its associated data.")
  setStoredLanguage("zh-TW")
  assert.match(uiText(notice), /將刪除 收件箱/)
  const warning = { key: "", lines: ["这封邮件还没有主题。", notice] }
  assert.equal(uiText(warning).split("\n").length, 2)
  setStoredLanguage("zh-CN")
  assert.equal(uiText(notice), "将删除 收件箱 及其关联数据。")
})

test("parameters are not recursively translated, interpolated or interpreted as markup", () => {
  const { uiMessage, uiText } = setup()
  const content = "收件箱 {1} <img src=x onerror=alert(1)> $&"
  assert.equal(uiText(uiMessage("{0}", [content])), content)
  assert.equal(uiText(uiMessage("邮件“{0}”将被删除。", [content])), `Message “${content}” will be deleted.`)
  assert.equal(uiText(" · 最近同步 {0}", ["09:30"]), " · Last synced 09:30")
})

test("counts use English singular/plural and locale-specific number formatting", () => {
  const { uiText } = setup()
  assert.equal(uiText("{0} 项", [1]), "1 item")
  assert.equal(uiText("{0} 项", [1200]), "1,200 items")
  assert.equal(uiText("已处理 {0} 封邮件", [1]), "Processed 1 message")
  assert.equal(uiText("{0} 个活跃邮箱", [1]), "1 active mailbox")
  assert.equal(uiText("{0} / {1} 个发送任务", [0, 1]), "0 / 1 send task")
  assert.equal(uiText("{0} 项", [0]), "0 items")
})

test("deferred date values and nested fallback labels follow the rendering language", () => {
  const { uiText, uiMessage, setStoredLanguage } = setup()
  const message = uiMessage("发送时间：{0}", [{ date: "2026-09-20T12:34:00Z" }])
  const english = uiText(message)
  setStoredLanguage("zh-TW")
  assert.notEqual(uiText(message), english)
  assert.equal(uiText(uiMessage("{0}", [uiMessage("无主题")])), "無主旨")
})

test("known errors retain actionable messages; unknown errors never leak server details", () => {
  const { load, uiText } = setup()
  const { ApiError, errorMessage } = load("src/lib/ui-errors.ts")
  const error = new ApiError(401, "邮箱或密码错误")
  assert.equal(error.status, 401)
  assert.equal(uiText(error.message), "Incorrect email or password")
  const secret = "SQL /private/mail/password=secret <script>alert(1)</script>"
  assert.equal(errorMessage(secret), "操作未完成，请稍后重试")
  assert.equal(new ApiError(403, secret).message, "无权执行此操作")
})

test("language remains usable when localStorage is blocked", () => {
  const { getInitialLanguage, setStoredLanguage, events } = setup("en", true)
  assert.equal(getInitialLanguage(), "en")
  setStoredLanguage("zh-TW")
  assert.equal(getInitialLanguage(), "zh-TW")
  assert.equal(events[0].type, "lanqin:language")
})

test("date and capacity formatting follows the selected language without changing values", () => {
  const { load, setStoredLanguage } = setup()
  const { formatDateTime, formatBytes } = load("src/lib/utils.ts")
  const date = "2026-09-20T12:34:00Z"
  for (const language of ["en", "zh-CN", "zh-TW"]) {
    setStoredLanguage(language)
    assert.equal(formatDateTime(date), new Intl.DateTimeFormat(language, {
      year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit",
    }).format(new Date(date)))
    assert.equal(formatBytes(1536), "1.5 KB")
  }
  assert.equal(formatDateTime("invalid"), "")
})

test("permissions are translated by stable keys, ignoring server-supplied display text", () => {
  const { load } = setup()
  const { permissionMessages, localizePermissionInfo } = load("src/lib/permission-translations.ts")
  assert.equal(Object.keys(permissionMessages).length, 47)
  const result = localizePermissionInfo({ key: "mail.access", label: "private", description: "private", category: "private" })
  assert.equal(result.label, "Access mailbox interface")
  assert.ok(!JSON.stringify(result).includes("private"))
})

test("DNS diagnostics preserve host parameters and safely handle unknown server messages", () => {
  const { load, setStoredLanguage } = setup()
  const { dnsCheckMessage } = load("src/lib/diagnostic-messages.ts")
  assert.equal(dnsCheckMessage("未找到HELO 主机 mail.example.test 的 SPF记录", false),
    "No SPF for HELO host mail.example.test record found")
  assert.equal(dnsCheckMessage("未找到 MX 记录", false), "No MX record found")
  assert.equal(dnsCheckMessage("untrusted internal error", false), "DNS checks failed")
  setStoredLanguage("zh-TW")
  assert.equal(dnsCheckMessage("DKIM 公钥匹配", true), "DKIM 公鑰相符")
})
