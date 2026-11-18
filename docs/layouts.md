# LanQin Email 前后台界面模板

## 管理员

在 **系统管理 → 系统设置 → 外观** 管理站点品牌、界面布局和配色：

- **站点品牌**：独立保存网站标题，上传或移除浏览器标签页的 ICO 图标。切换模板、恢复经典或修改配色不会重置站点品牌。
- **界面模板管理**：分别选择用户前台和管理后台模板。
- **外观主题**：设置全站默认配色。用于通知和发信的内容仍在「邮件模板」，不是界面模板。

「应用模板」只修改这一侧；为两侧选择同一个兼容模板后，可点「应用到前台和后台」一次提交。
「恢复经典模板」仅重置这一侧。两套模板设置和位于下方的配色设置互相独立，
不会覆盖用户自己的深浅色偏好。

- **前台经典**：保留原有文件夹侧栏和可调整宽度的列表/阅读区域。
- **前台工作台**：桌面把常用文件夹与账户操作上移，同时保留共享/外部邮箱和标签入口；邮件列表和阅读区域仍来自主程序。移动端继续使用原有单列列表/阅读交互。
- **后台经典**：保留原有侧栏与概览排布。
- **后台工作台**：桌面和移动都改用顶部导航，重新编排概览区域；用户、域名与设置等业务功能继续由主程序渲染。

预览是使用合成数据绘制的布局示意，不读取邮件或保存模板。可在对话框切换示意页面、手机尺寸与深浅色。
只有具备设置修改权限的管理员能应用。模板未安装或运行失败时回退经典；如果后台界面无法使用，
可直接打开 `/admin?section=settings&tab=appearance&layout=classic`。
该地址临时强制本页使用经典布局，但仍需登录和设置权限，不会自行写入配置。进入后可以恢复后台经典模板。
缺省或升级不自动启用工作台。布局保存后，已打开的邮箱或管理区保留当前布局，防止写信、配置或个人资料编辑被打断。
重新打开对应区域或刷新页面时加载最新布局；后台修改后也可以直接在新标签页打开已保存的布局。

## 开发主题

复制 [工作台的模板清单](../apps/web/src/themes/layouts/packs/workbench/theme.json)，为其选择唯一小写 ID，
填写名称、作者、`apiVersion: 1` 和支持的 `surfaces`（`front`、`admin` 或两者）。
把清单导入 [目录](../apps/web/src/themes/layouts/catalog.ts)；每个支持的区域需注册对应的实现。
实现可直接参考 [前台对照组件](../apps/web/src/themes/layouts/front/front-layout.tsx) 和
[后台工作台组件](../apps/web/src/themes/layouts/admin/workbench.tsx)，在
[前台注册表](../apps/web/src/themes/layouts/front/registry.ts) 或
[后台注册表](../apps/web/src/themes/layouts/admin/registry.ts) 登记同一个 ID：

- 前台的桌面页面接收宿主已经准备好的导航、邮箱身份、操作条、搜索和内容；
  阅读区域接收列表和阅读两个插槽。可以移动它们，不要复制 MailPage 的业务状态。
  登录、注册仍使用宿主表单和验证流程；移动邮件流程不由桌面布局替换。
- 后台的 Shell 接收已过滤的导航链接、身份和内容；Overview 接收指标、服务状态、引导、域名和运行状况。
  系统设置、用户管理、备份恢复等保留宿主表单与权限。按现有接口为模板登记 Shell 和 Overview。
- 可选颜色数据沿用 [配色主题契约](themes.md)。不需要修改 Go API 或邮件数据库。
- 名称和交互标签要纳入原有字典（简体、繁体、英文）；提供键盘和移动端支持。
  使用现有 shadcn 组件，保持样式作用域，不覆盖宿主全局样式。
- 模板源码与宿主同一构建产物，TypeScript 接口不是沙箱。只构建经过维护者审核的模板，
  不接受后台上传脚本、外站模板或由模板直接访问邮件 API、凭据或任意网络资源。
- 合并前运行 `pnpm --dir apps/web run check`，用两个模板验证登录、直达页面、移动端、
  用户/只读管理员权限、后台恢复，以及实际页面与预览示意一致。

## 内部接口与持久化

- `GET /api/public/settings` 增加 `frontLayoutId`，不公开后台模板 ID。原 `themeId` 配色字段保持兼容。
- `GET /api/me/site-layout` 向可进入管理区的已认证账号提供 `adminLayoutId`，不是配置写入接口。
- `GET /api/admin/settings/layouts` 返回两侧设置，需要 `admin.settings.view`。
- `POST /api/admin/settings/layouts` 需要 `admin.settings.update`，至少提供 `frontLayoutId`
  或 `adminLayoutId` 一项。只更新提交的区域；同时提交则作为一项设置原子保存。
  持久化使用已有 `system_settings`，没有邮件数据库迁移。
- `POST /api/admin/settings/branding` 需要 `admin.settings.update`，只保存 `siteTitle`。
  图标沿用原有上传和移除接口；布局和配色接口都不写入品牌设置。
- 主题只改变 Web 页面，不涉及邮件正文、通知邮件模板或 SMTP 行为。
