# LanQin Email 界面主题（契约 v1）
本文描述配色层。独立前后台布局见 [界面模板和开发指南](layouts.md)。

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

1. 复制 [暖纸主题的 `theme.json`](../apps/web/src/themes/paper/theme.json)
   到 `apps/web/src/themes/<你的主题 id>/theme.json`，这是完整、可用的模板。
2. 填写 `version: 1`、唯一的 `id` 和显示名称 `name`。ID 必须为 1–48 个小写
   ASCII 字符，以小写字母开头，可用数字和单连字符分隔；`classic` 是保留回退标识。
   `name` 使用简体中文词条，在 [现有字典](../apps/web/src/lib/ui-translations.json)
   同时登记 `zh-TW` 和 `en`，不要直接改已发布的主题 ID。
3. 在 `light` 和 `dark` 对象中只填需要覆盖的视觉值；两个对象都必须存在，可用 `{}`。
   未覆写的值继承同一明暗模式的 [经典主题](../apps/web/src/themes/classic/styles.css)，
   不会从另一个明暗模式继承。示例：

   ```json
   {
     "version": 1,
     "id": "my-paper",
     "name": "我的纸色主题",
     "light": { "background": "38 27% 96%", "radius": "0.8rem" },
     "dark": { "background": "29 14% 12%", "radius": "0.8rem" }
   }
   ```

4. 在 [主题目录](../apps/web/src/themes/catalog.ts) 导入 JSON，将路径与数据加入
   `registeredPacks`。Vite 会验证清单并生成静态作用域 CSS，无需修改 Go 代码。
5. 运行 `pnpm --dir apps/web run check`，包含 `check:themes` 自动化测试。
   手动检查浅色/深色、移动端、焦点样式、键盘操作及文字对比度，并检查登录、写信、邮件列表、
   共享/外部邮箱和管理页是否可读。

### 可依赖的界面接口

- 键名不写 CSS 的 `--` 前缀，例如用 `background`，不是 `--background`。
- 配色：`background`、`foreground`、`card`、`popover`、`primary`、`secondary`、
  `muted`、`accent`、`destructive` 及经典主题中这些配色已有的配对前景色。
- 控件：`border`、`input`、`ring`、`radius`；侧栏：经典主题中已有的 `sidebar-*`。
- [完整键名及输入限制](../apps/web/src/themes/contract.ts)：颜色是 HSL 通道，范围为
  `0–360 0–100% 0–100%`，例如 `38 27% 96%`；圆角为 `0rem` 至 `2rem`。
  不接受原始 CSS、变量引用、函数、URL 或新增选择器；重复键也会使构建失败。
- v1 只提供展示样式，不替换认证、路由、邮件编辑器或业务组件，也不改变 SMTP 邮件模板和
  沙箱 iframe 中的正文。页面 DOM、组件名和内部样式类不属于主题接口。

为了避免未经审核的代码或样式接触邮箱页面，不支持后台上传或从网络动态加载主题。
仅维护者审核后随前端构建发布的主题才会实际应用。后端只保存一个经过格式校验的标识，
不尝试按 ID 查找文件、加载资源或判断安装状态。卸载主题可以移除注册项及文件；
还未切换的实例会安全回退，管理员可随后关闭自定义主题来清除旧选择。

### 浏览器接口（不是开放集成 API）

- `GET /api/public/settings` 的 `themeId` 字段供所有页面（包括未登录页面）使用。
- `GET /api/admin/settings/theme`：返回 `{"themeId":"classic"}` 或当前保存的 ID。
- `POST /api/admin/settings/theme`：接受 `{"themeId":"paper"}` 并返回保存值；
  写入 `{"themeId":"classic"}` 即关闭自定义风格。该接口不会修改 SMTP、认证、收发设置。
