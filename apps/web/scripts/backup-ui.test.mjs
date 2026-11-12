import assert from "node:assert/strict"
import test from "node:test"
import { createRequire } from "node:module"
import React from "react"
import { renderToStaticMarkup } from "react-dom/server"
import { sourceLoader } from "./i18n-test-loader.mjs"

// Match the CommonJS dependency instance used by the source loader.
const { QueryClient, QueryClientProvider } = createRequire(import.meta.url)("@tanstack/react-query")

// These checks render real components with a synthetic query cache. They do not
// exercise browser interaction, network requests, or the running API.
function setup(language = "en") {
  const load = sourceLoader({
    window: { navigator: { language }, localStorage: { getItem: () => language } },
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } },
  })
  const destination = {
    id: "destination-test",
    name: "Synthetic destination",
    type: "s3",
    disabled: false,
    credentialsSet: true,
  }
  const userId = "synthetic-admin"
  const set = (key, value) => client.setQueryData(["admin", key, userId], value)
  set("backup-capabilities", {
    onlineBackupAvailable: true,
    recipient: "password:synthetic-generation",
  })
  const render = (filename, name, props) =>
    renderToStaticMarkup(
      React.createElement(
        QueryClientProvider,
        { client },
        React.createElement(load(filename)[name], props)
      )
    )
  return { load, client, userId, destination, set, render }
}

for (const language of ["zh-CN", "zh-TW", "en"]) {
  test(`password encryption status is translated and never displays its internal recipient (${language})`, () => {
    const env = setup(language)
    env.set("backup-encryption", {
      mode: "password",
      configured: true,
      passwordSet: true,
      recipient: "password:synthetic-generation",
      revision: "test",
      environmentKeyConfigured: false,
    })
    const markup = env.render(
      "src/components/backup-encryption-settings.tsx",
      "BackupEncryptionSettings",
      { userId: env.userId, editable: true }
    )
    assert.ok(markup.includes(env.load("src/lib/language.tsx").uiText("更改备份密码")))
    assert.ok(!markup.includes("synthetic-generation"))
    assert.ok(!markup.includes('type="password"'))
    env.client.clear()
  })
  test(`new backups require an administrator password, including unconfigured default and legacy-key instances (${language})`, () => {
    const env = setup(language)
    const text = env.load("src/lib/language.tsx").uiText
    for (const mode of ["password", "key"]) {
      env.set("backup-encryption", {
        mode,
        configured: mode === "key",
        passwordSet: false,
        recipient: "synthetic-legacy-public-key",
        revision: "test",
        environmentKeyConfigured: true,
      })
      const markup = env.render(
        "src/components/backup-encryption-settings.tsx",
        "BackupEncryptionSettings",
        { userId: env.userId, editable: true }
      )
      assert.ok(markup.includes(text("尚未设置备份密码")))
      assert.ok(markup.includes(text("设置备份密码")))
      assert.ok(
        markup.includes(
          text("必须先由超级管理员设置备份密码，才能新建备份或启用计划；服务器不会自动生成备份密码")
        )
      )
      assert.ok(markup.includes('role="alert"'))
      assert.ok(!markup.includes(text("高级公钥模式")))
      assert.ok(!markup.includes("synthetic-legacy-public-key"))
      assert.doesNotMatch(markup, /disabled=/)
      const readonly = env.render(
        "src/components/backup-encryption-settings.tsx",
        "BackupEncryptionSettings",
        { userId: env.userId, editable: false }
      )
      assert.match(readonly, /disabled=/)
    }
    env.client.clear()
  })
}

test("each destination has a manual backup button, unavailable to read-only or disabled destinations", () => {
  const env = setup()
  const filename = "src/components/backup-run-now-button.tsx"
  const base = {
    userId: env.userId,
    destination: env.destination,
    revision: "test",
    editable: true,
    onCreated() {},
  }
  const active = env.render(filename, "BackupRunNowButton", base)
  assert.match(active, /Run now/)
  assert.doesNotMatch(active, /disabled=/)
  assert.match(
    env.render(filename, "BackupRunNowButton", { ...base, editable: false }),
    /disabled=/
  )
  assert.match(
    env.render(filename, "BackupRunNowButton", {
      ...base,
      destination: { ...env.destination, disabled: true },
    }),
    /disabled=/
  )
  env.set("backup-capabilities", {
    onlineBackupAvailable: true,
    recipient: "synthetic-legacy-public-key",
  })
  assert.match(env.render(filename, "BackupRunNowButton", base), /disabled=/)
  env.set("backup-capabilities", { onlineBackupAvailable: false })
  assert.match(env.render(filename, "BackupRunNowButton", base), /disabled=/)
  env.client.clear()
})

test("calendar and fixed interval schedules render their saved period and fields", () => {
  const env = setup()
  const props = {
    userId: env.userId,
    destination: env.destination,
    destinationsRevision: "test",
    editable: true,
  }
  for (const [schedule, text] of [
    [{ period: "hourly", minute: 15 }, "Hourly"],
    [{ period: "weekly", time: "03:00", weekdays: [1, 5] }, "Monday、Friday"],
    [{ period: "monthly", time: "03:00", dayOfMonth: 31 }, "Day 31 of each month"],
    [{ period: "interval", interval: 5, unit: "minutes" }, "Every N minutes"],
    [{ time: "03:00" }, "Daily"],
  ]) {
    env.set("backup-schedules", {
      revision: "test",
      items: [{ destinationId: env.destination.id, enabled: true, timezone: "UTC", ...schedule }],
    })
    const markup = env.render(
      "src/components/backup-schedule-panel.tsx",
      "BackupSchedulePanel",
      props
    )
    assert.ok(markup.includes(text), text)
    assert.doesNotMatch(markup, /undefined/)
  }
  env.client.clear()
})

test("encryption API errors stay actionable and switch languages at render time", () => {
  const env = setup()
  const { errorMessage } = env.load("src/lib/ui-errors.ts")
  const { uiText } = env.load("src/lib/language.tsx")
  const error = errorMessage("backup_encryption_current_password_invalid")
  assert.equal(uiText(error, undefined, "en"), "The current backup password is incorrect")
  assert.equal(uiText(error, undefined, "zh-TW"), "目前備份密碼錯誤")
  assert.equal(uiText(error, undefined, "zh-CN"), "当前备份密码错误")
  env.client.clear()
})

test("new backup passwords count Unicode characters rather than bytes or UTF-16 units", () => {
  const env = setup()
  const { validNewBackupPassword } = env.load("src/lib/backup-password.ts")
  for (const password of [
    "abcdefgh",
    "備份密碼測試八字",
    "🔐".repeat(8),
    "x".repeat(128),
    "🔐".repeat(128),
    "  abcdef  ",
  ]) {
    assert.equal(validNewBackupPassword(password), true)
  }
  for (const password of [
    "abcdefg",
    "密".repeat(7),
    "🔐".repeat(7),
    "x".repeat(129),
    "密".repeat(129),
    "🔐".repeat(129),
    " ".repeat(8),
    "\u0085".repeat(8),
    "abcdefgh\x00",
    "abcdefgh\u007f",
    "abcdefgh\ud800",
  ]) {
    assert.equal(validNewBackupPassword(password), false)
  }
  const { uiText } = env.load("src/lib/language.tsx")
  for (const language of ["zh-CN", "zh-TW", "en"]) {
    const message = uiText("备份密码须为 8 至 128 个字符，且两次输入必须相同", undefined, language)
    assert.match(message, /8/)
    assert.match(message, /128/)
    assert.doesNotMatch(message, /1024|字节|位元組|bytes/)
  }
  env.client.clear()
})

for (const language of ["zh-CN", "zh-TW", "en"]) {
  test(`maintenance progress shows real stages and disconnected last-known state (${language})`, () => {
    const env = setup(language)
    const text = env.load("src/lib/language.tsx").uiText
    const filename = "src/components/backup-maintenance-panel.tsx"
    const base = {
      requestId: "0123456789abcdef0123456789abcdef",
      sourceJobId: "bkj_synthetic",
      disconnected: false,
      fetching: false,
      submitting: false,
      receivedAt: Date.parse("2026-11-12T05:10:15Z"),
      onRetry() {},
    }
    const status = {
      version: 1,
      id: base.requestId,
      planId: "synthetic-plan",
      webJob: base.sourceJobId,
      state: "maintaining",
      phase: "maintenance",
      createdAt: "2026-11-12T05:10:00Z",
      updatedAt: "2026-11-12T05:10:15Z",
      progress: { step: "installing", startedAt: "2026-11-12T05:10:10Z" },
    }
    const running = env.render(filename, "BackupMaintenanceProgress", { ...base, status })
    assert.ok(running.includes(text("替换数据库及邮件文件")))
    assert.ok(running.includes(text("恢复进度")))
    assert.match(running, /aria-current="step"/)
    assert.ok(running.includes(text("尚未执行")))
    assert.doesNotMatch(running, /[0-9]+%/)
    const disconnected = env.render(filename, "BackupMaintenanceProgress", {
      ...base,
      status,
      disconnected: true,
    })
    assert.ok(disconnected.includes(text("状态连接已中断")))
    assert.ok(disconnected.includes(text("上次确认的阶段")))
    assert.ok(disconnected.includes("00:00:15"))
    assert.ok(disconnected.includes("00:00:05"))
    assert.ok(!disconnected.includes(text("正在维护并覆盖恢复")))
    assert.ok(disconnected.includes(text("上次确认的阶段耗时")))
    const stalledStage = env.render(filename, "BackupMaintenanceProgress", {
      ...base,
      status: {
        ...status,
        updatedAt: "2026-11-12T05:10:01Z",
        progress: { step: "installing", startedAt: "2026-11-12T05:10:01Z" },
      },
      receivedAt: Date.parse("2026-11-12T05:10:20Z"),
      disconnected: true,
    })
    assert.ok(stalledStage.includes("00:00:20"))
    assert.ok(stalledStage.includes("00:00:19"))
    assert.ok(!stalledStage.includes("00:00:01"))
    const unauthorized = env.render(filename, "BackupMaintenanceProgress", {
      ...base,
      status,
      disconnected: true,
      statusUnauthorized: true,
    })
    assert.ok(
      unauthorized.includes(
        text("登录会话已失效，请重新登录后回到备份页面查询此请求；请求编号已保留，请勿重复提交")
      )
    )
    const succeeded = env.render(filename, "BackupMaintenanceProgress", {
      ...base,
      disconnected: true,
      status: {
        ...status,
        state: "succeeded",
        phase: "completed",
        updatedAt: "2026-11-12T05:11:17Z",
        progress: { step: "completed", startedAt: "2026-11-12T05:11:17Z" },
      },
    })
    assert.ok(succeeded.includes("00:01:17"))
    assert.ok(succeeded.includes(text("覆盖恢复完成，请重新登录")))
    assert.ok(!succeeded.includes(text("状态连接已中断")))
    assert.ok(!succeeded.includes(text("尚未执行")))
    const unknown = env.render(filename, "BackupMaintenanceProgress", {
      ...base,
      status: {
        ...status,
        progress: { step: "/private/credential", startedAt: "invalid" },
        phase: "private-phase",
        errorCode: "private-error",
      },
    })
    assert.ok(
      !unknown.includes("/private/credential") &&
        !unknown.includes("private-phase") &&
        !unknown.includes("private-error")
    )
    assert.ok(unknown.includes(text("等待状态更新")))
    assert.ok(!unknown.includes('aria-label="' + text("恢复进度") + '"'))
    env.client.clear()
  })
}

test("maintenance elapsed time and legacy progress fallback are bounded and never infer later completion", () => {
  const env = setup()
  const { backupMaintenanceElapsed, backupMaintenanceStep } = env.load(
    "src/lib/backup-maintenance.ts"
  )
  assert.equal(
    backupMaintenanceElapsed("2026-11-12T00:00:00Z", Date.parse("2026-11-12T01:02:03Z")),
    "01:02:03"
  )
  assert.equal(
    backupMaintenanceElapsed("2026-11-12T00:00:00Z", Date.parse("2026-11-11T23:59:00Z")),
    "00:00:00"
  )
  assert.equal(backupMaintenanceElapsed("invalid", Date.now()), undefined)
  assert.equal(backupMaintenanceElapsed(undefined, NaN), undefined)
  assert.equal(backupMaintenanceStep({ state: "maintaining", phase: "maintenance" }).milestone, -1)
  assert.equal(
    backupMaintenanceStep({
      state: "maintaining",
      phase: "maintenance",
      progress: { step: "rolling-back" },
    }).milestone,
    -1
  )
  assert.equal(
    backupMaintenanceStep({ state: "failed", phase: "completed", progress: { step: "completed" } })
      .milestone,
    -1
  )
  assert.equal(backupMaintenanceStep({ state: "succeeded" }).milestone, 6)
  env.client.clear()
})
