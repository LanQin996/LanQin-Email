# Web shadcn/ui 规则

`apps/web` 的业务页面和业务组件必须优先并完整使用官方 shadcn/ui 组件源码。

## 规则

- 所有 UI primitive 必须来自 `@/components/ui/*`。
- 新增 UI 能力时，先执行 `npx shadcn@latest add <component>` 添加官方组件源码。
- 业务 TSX 禁止直接写原生 `<button>`、`<input>`、`<textarea>`、`<select>`、`<table>`、`<dialog>`、`<aside>` 等控件。
- 业务页面不要在标题下方添加说明性小字/副标题文案，例如“管理当前用户资料和密码”“管理当前用户拥有的多个邮箱”这类内容。
- 业务 TSX 禁止使用 `CardDescription`、`DialogDescription`、`SheetDescription`；需要说明时改为清晰标题、表单 Label、按钮或 Badge。
- 视觉风格必须保持 shadcn `new-york + neutral`：白底、黑色主按钮、细边框、克制留白。
- 禁止在业务 TSX 使用蓝色品牌色、渐变背景、重阴影/营销感样式，例如 `bg-gradient-*`、`*-blue-*`、`shadow-xl`、`shadow-2xl`、`shadow-primary*`。
- `src/components/ui/**` 是官方 shadcn 组件源码，允许内部使用原生标签。
- 允许语义/布局标签：`div`、`span`、`form`、`header`、`main`、`section`、`p`、标题、`a`。

## 检查

```bash
cd apps/web
pnpm run check:shadcn
pnpm run build
```
