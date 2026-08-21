#!/usr/bin/env bash

set -euo pipefail

output_file="${1:-release-notes.md}"
tag="${RELEASE_TAG:-${GITHUB_REF_NAME:-}}"
version="${RELEASE_VERSION:-${tag#v}}"
repo="${REPOSITORY:-${GITHUB_REPOSITORY:-}}"
registry="${REGISTRY:-ghcr.io}"

if [[ -z "${tag}" || -z "${repo}" ]]; then
	echo "RELEASE_TAG and REPOSITORY are required" >&2
	exit 1
fi

repo_url="https://github.com/${repo}"
image_base="${registry}/${repo}"
image_base="${image_base,,}"
current_commit="$(git rev-list -n 1 "${tag}")"
previous_tag="$(git describe --tags --abbrev=0 "${current_commit}^" 2>/dev/null || true)"
manual_notes=".github/release-notes/${tag}.md"

work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT

category_order=(breaking security features fixes performance improvements docs maintenance other)
declare -A category_titles=(
	[breaking]="破坏性变更"
	[security]="安全更新"
	[features]="新增功能"
	[fixes]="问题修复"
	[performance]="性能优化"
	[improvements]="优化改进"
	[docs]="文档更新"
	[maintenance]="工程维护"
	[other]="其他更新"
)
declare -A category_counts=()

for category in "${category_order[@]}"; do
	category_counts["${category}"]=0
	: >"${work_dir}/${category}.md"
done

if [[ -n "${previous_tag}" ]]; then
	commit_range="${previous_tag}..${tag}"
else
	commit_range="${tag}"
fi

commit_count=0
conventional_pattern='^([[:alnum:]_-]+)(\(([^)]*)\))?(!)?:[[:space:]]*(.+)$'
while IFS=$'\t' read -r hash subject; do
	[[ -z "${hash}" || -z "${subject}" ]] && continue

	type=""
	scope=""
	breaking_marker=""
	description="${subject}"
	if [[ "${subject}" =~ ${conventional_pattern} ]]; then
		type="${BASH_REMATCH[1],,}"
		scope="${BASH_REMATCH[3]}"
		breaking_marker="${BASH_REMATCH[4]}"
		description="${BASH_REMATCH[5]}"
	fi

	if [[ "${type}" == "release" || "${subject}" == chore\(release\):* ]]; then
		continue
	fi

	lower_subject="${subject,,}"
	category="other"
	if [[ -n "${breaking_marker}" ]] || git show -s --format=%B "${hash}" | grep -Eq '^BREAKING[ -]CHANGE:'; then
		category="breaking"
	elif [[ "${type}" == "security" || "${lower_subject}" == *security* || "${subject}" == *安全* || "${subject}" == *漏洞* ]]; then
		category="security"
	else
		case "${type}" in
		feat | feature)
			category="features"
			;;
		fix | bugfix | hotfix)
			category="fixes"
			;;
		perf | performance)
			category="performance"
			;;
		refactor | improve | improvement | style)
			category="improvements"
			;;
		docs | doc)
			category="docs"
			;;
		build | chore | ci | deps | dependency | test | tests)
			category="maintenance"
			;;
		esac
	fi

	description="${description//$'\r'/}"
	description="${description//[/\\[}"
	description="${description//]/\\]}"
	scope="${scope//\`/}"
	short_hash="${hash:0:7}"
	if [[ -n "${scope}" ]]; then
		scope_prefix="**${scope}**："
	else
		scope_prefix=""
	fi

	printf -- '- %s%s ([`%s`](%s/commit/%s))\n' \
		"${scope_prefix}" "${description}" "${short_hash}" "${repo_url}" "${hash}" \
		>>"${work_dir}/${category}.md"
	category_counts["${category}"]=$((category_counts["${category}"] + 1))
	commit_count=$((commit_count + 1))
done < <(git log --no-merges --reverse --format='%H%x09%s' "${commit_range}")

# A tag range containing only merge commits is rare, but still deserves useful notes.
if ((commit_count == 0)); then
	while IFS=$'\t' read -r hash subject; do
		[[ -z "${hash}" || -z "${subject}" ]] && continue
		short_hash="${hash:0:7}"
		printf -- '- %s ([`%s`](%s/commit/%s))\n' \
			"${subject}" "${short_hash}" "${repo_url}" "${hash}" >>"${work_dir}/other.md"
		category_counts[other]=$((category_counts[other] + 1))
		commit_count=$((commit_count + 1))
	done < <(git log --first-parent --reverse --format='%H%x09%s' "${commit_range}")
fi

contributors_count="$(git log --no-merges --format='%aN' "${commit_range}" | sed '/^$/d' | sort -fu | wc -l | tr -d ' ')"
overview_parts=()
for category in "${category_order[@]}"; do
	count="${category_counts[${category}]}"
	if ((count > 0)); then
		overview_parts+=("${category_titles[${category}]} ${count} 项")
	fi
done
overview=""
for part in "${overview_parts[@]}"; do
	if [[ -z "${overview}" ]]; then
		overview="${part}"
	else
		overview="${overview}，${part}"
	fi
done

{
	echo "> LanQin Email 是一套自建邮箱 Webmail 全栈方案，集成 Web、API、Postfix、Dovecot 与 Rspamd。"
	echo

	if [[ -s "${manual_notes}" ]]; then
		echo "## 版本摘要"
		echo
		cat "${manual_notes}"
		echo
	fi

	echo "## 版本概览"
	echo
	if ((commit_count > 0)); then
		echo "本版本包含 **${commit_count} 项提交变更**：${overview}。"
	else
		echo "本版本没有可列出的提交变更。"
	fi
	echo

	echo "## 变更详情"
	echo
	for category in "${category_order[@]}"; do
		if ((category_counts["${category}"] == 0)); then
			continue
		fi
		echo "### ${category_titles[${category}]}"
		echo
		cat "${work_dir}/${category}.md"
		echo
	done

	echo "## 升级说明"
	echo
	echo "> 生产环境升级前，请备份 \`deploy/.env\`、数据库、Maildir 邮件目录与 DKIM 私钥。"
	echo
	if ((category_counts[breaking] > 0)); then
		echo "本版本包含标记为破坏性变更的提交，请先阅读上方“破坏性变更”并在测试环境验证。"
	else
		echo "本版本未检测到使用 Conventional Commits 标记的破坏性变更；升级前仍建议核对配置差异。"
	fi
	echo
	echo "使用指定版本镜像升级 all-in-one 部署："
	echo
	echo '```bash'
	echo 'cd deploy'
	echo "export LANQIN_IMAGE=\"${image_base}:${tag}\""
	echo 'docker compose pull'
	echo 'docker compose up -d'
	echo 'docker compose ps'
	echo 'docker compose logs --tail=100 lanqin-email'
	echo '```'
	echo
	echo "数据库结构由应用启动时自动检查和迁移。跨 SQLite、MySQL、PostgreSQL 的数据库迁移不属于版本升级流程，请勿直接切换数据库类型。"
	echo

	echo "## Docker 镜像"
	echo
	echo "发布平台：\`linux/amd64\`"
	echo
	echo '| 组件 | 固定版本镜像 |'
	echo '| --- | --- |'
	echo "| All-in-one | \`${image_base}:${tag}\` |"
	echo "| API | \`${image_base}-api:${tag}\` |"
	echo "| Web | \`${image_base}-web:${tag}\` |"
	echo "| Postfix | \`${image_base}-postfix:${tag}\` |"
	echo "| Dovecot | \`${image_base}-dovecot:${tag}\` |"
	echo "| Rspamd | \`${image_base}-rspamd:${tag}\` |"
	echo
	echo "同一构建还会发布 \`${version}\`、\`latest\` 和 \`sha-*\` 标签。生产环境建议固定使用 \`${tag}\`。"
	echo

	echo "## 发布验证"
	echo
	echo "创建 Release 前，发布工作流必须完成："
	echo
	echo "- Web 端 shadcn/ui 规则检查与 TypeScript/Vite 生产构建"
	echo "- Go API 全量测试"
	echo "- All-in-one、API、Web、Postfix、Dovecot、Rspamd 六个镜像构建并推送至 GHCR"
	echo

	echo "<details>"
	echo "<summary>公网邮件服务部署检查项</summary>"
	echo
	echo "- 配置 MX、SPF、DKIM、DMARC 与正确的 PTR/rDNS。"
	echo "- 放行并检查 25、465、587、993、995 等所需端口。"
	echo "- 为 Web、SMTP、IMAP、POP3 配置匹配域名的有效 TLS 证书。"
	echo "- 升级后通过后台 SMTP 测试和真实外部邮箱验证收发链路。"
	echo
	echo "</details>"
	echo

	echo "## 文档"
	echo
	echo "- [中文说明](${repo_url}/blob/${tag}/README.zh-CN.md)"
	echo "- [部署文档](${repo_url}/blob/${tag}/deploy/README.md)"
	echo "- [开放 API](${repo_url}/blob/${tag}/docs/API.md)"
	echo "- [开源协议](${repo_url}/blob/${tag}/LICENSE)"
	echo

	echo "## 完整变更"
	echo
	if [[ -n "${previous_tag}" ]]; then
		echo "- 版本对比：[${previous_tag}...${tag}](${repo_url}/compare/${previous_tag}...${tag})"
	else
		echo "- 当前提交：[\`${current_commit:0:7}\`](${repo_url}/commit/${current_commit})"
	fi
	echo "- 提交数量：${commit_count}"
	echo "- 贡献者：${contributors_count}"
} >"${output_file}"

echo "Generated ${output_file} for ${tag} (${commit_count} commits)"
