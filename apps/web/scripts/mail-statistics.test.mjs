import assert from "node:assert/strict"
import test from "node:test"
import React from "react"
import { renderToStaticMarkup } from "react-dom/server"
import { sourceLoader } from "./i18n-test-loader.mjs"

const empty = {
  totalMessages: 0, unreadMessages: 0, starredMessages: 0,
  attachmentCount: 0, attachmentBytes: 0, storageBytes: 0,
  quotaBytes: 0, quotaUsedPct: 0, byFolder: [],
}

function render(language, overrides = {}) {
  const load = sourceLoader({ window: {
    localStorage: { getItem: () => language },
    navigator: { language },
  } })
  const { MailStatistics } = load("src/components/mail-statistics.tsx")
  return renderToStaticMarkup(React.createElement(MailStatistics, {
    stats: { ...empty, ...overrides }, folderLabel: (name) => name,
  }))
}

test("empty mailbox renders localized empty state without invalid ratios", () => {
  for (const [language, label] of [["zh-CN", "暂无邮件数据"], ["zh-TW", "暫無郵件資料"], ["en", "No message data"]]) {
    const html = render(language)
    assert.ok(html.includes(label))
    assert.ok(!html.includes("NaN"))
    assert.ok(!html.includes("Infinity"))
    assert.ok(html.includes('stroke-dasharray="0 100"'))
  }
})

test("statistics use message counts, escape folder names, and clamp full storage", () => {
  const html = render("en", {
    totalMessages: 10, unreadMessages: 2, storageBytes: 2048, quotaBytes: 1024,
    byFolder: [{ folder: '<img src=x onerror=alert(1)>', role: "inbox", count: 10, unread: 2, bytes: 2048 }],
  })
  assert.ok(html.includes('stroke-dasharray="80 100"'))
  assert.ok(html.includes("Storage limit reached"))
  assert.ok(html.includes("Available storage: 0 B"))
  assert.ok(html.includes("width:100%"))
  assert.ok(!html.includes("width:200%"))
  assert.ok(html.includes("&lt;img"))
  assert.ok(!html.includes("<img"))
})

test("quota warning starts at ninety percent and absent quota has no false health claim", () => {
  assert.ok(render("en", { storageBytes: 90, quotaBytes: 100 }).includes("Storage almost full"))
  assert.ok(render("en", { storageBytes: 89, quotaBytes: 100 }).includes("Storage usage is healthy"))
  const html = render("en", { storageBytes: 90 })
  assert.ok(html.includes("No storage limit set"))
  assert.ok(!html.includes("Storage usage is healthy"))
})
