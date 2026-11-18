# LanQin Email 界面主题（契约 v1）

## 管理员：开启、切换或关闭

打开 **系统管理 → 系统设置 → 外观**，选中一个主题并点击 **应用主题**。
同一个实例只有一种生效的界面风格，适用于登录、注册、邮箱、个人设置和管理页面。
点击 **关闭自定义主题** 即恢复内置的 **经典主题**，不必重新部署。
读取此设置需要 `admin.settings.view`，修改需要 `admin.settings.update`。

新装和升级实例默认沿用经典主题。配置保存在现有 `system_settings` 表，不需要数据库迁移。
如果配置指向当前构建未包含的主题，网页仍可打开并使用经典风格，管理页会提示回退。
打开的其他网页会在重新进入、窗口重新获得焦点且数据过期、或前台轮询时读取更新；
邮箱和个人设置里的明暗模式仍由各浏览器保存，不会被站点主题覆盖。

## 开发者：用暖纸主题做起点

1. 复制 [暖纸主题](../apps/web/src/themes/paper/) 到
   `apps/web/src/themes/<你的主题 id>/`。
2. 修改 `manifest.ts`，通过 [`defineTheme`](../apps/web/src/themes/contract.ts) 声明
   `version: 1`、唯一 `id` 和 `name`。ID 必须为 1–48 个小写 ASCII
   字符，由小写字母开头，可用数字和单连字符分隔；`classic` 是保留回退标识。
   `name` 使用简体中文辞条，在 [现有字典](../apps/web/src/lib/ui-translations.json)
   同时登记 `zh-TW` 和 `en`。不要直接改已发布的主题 ID。
3. 在 `styles.css` 中只覆盖所需视觉变量，选择器使用
   `html[data-lanqin-theme="<你的主题 id>"]`；暗色覆写用
   `html.dark[data-lanqin-theme="<你的主题 id>"]`。
   未覆写的变量继承 [经典主题](../apps/web/src/themes/classic/styles.css)。
4. 在 [主题注册表](../apps/web/src/themes/registry.ts) 导入清单和 CSS，并把清单加入
   `themes`。重新构建后即可在管理页看到主题，不需要增加 Go 代码。
5. 运行 `pnpm --dir apps/web run check`。手动检查浅色/深色、移动端、焦点样式
   和键盘操作，并检查登录、写信、邮件列表、共享/外部邮箱和管理页是否可读。

### 可依赖的界面接口

- 配色：`--background`、`--foreground`、`--card`、`--popover`、`--primary`、
  `--secondary`、`--muted`、`--accent`、`--destructive` 及经典主题已定义的配对前景色。
- 控件：`--border`、`--input`、`--ring`、`--radius`。
- 侧栏：经典主题定义的 `--sidebar-*` 变量。
- 颜色使用无 `hsl()` 包装的 HSL 通道，例如 `38 27% 96%`；圆角使用 CSS 长度。
  页面 DOM、业务组件名和内部类名不属于主题接口。
- v1 提供展示样式，不替换认证、路由、邮件编辑器或业务组件，也不改变 SMTP 邮件模板和
  沙箱 iframe 中的正文。不要在主题引入远程样式、字体、追踪像素或脚本。

为了避免未经审核的代码或样式接触邮箱页面，不支持后台上传或从网络动态加载主题。
仅维护者审核后随前端构建发布的主题才会实际应用。后端只保存一个经过格式校验的标识，
不尝试按 ID 查找文件、加载资源或判断安装状态。卸载主题可以移除注册项及文件；
还未切换的实例会安全回退，管理员可随后关闭自定义主题来清除旧选择。

### 浏览器接口（不是开放集成 API）

- `GET /api/public/settings` 的 `themeId` 字段供所有页面（包括未登录页面）使用。
- `GET /api/admin/settings/theme`：返回 `{"themeId":"classic"}` 或当前保存的 ID。
- `POST /api/admin/settings/theme`：接受 `{"themeId":"paper"}` 并返回保存值；
  写入 `{"themeId":"classic"}` 即关闭自定义风格。该接口不会修改 SMTP、认证、收发设置。
