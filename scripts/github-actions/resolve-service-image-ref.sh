#!/usr/bin/env bash
set -euo pipefail

image_repo="${SERVICE_REGISTRY:?SERVICE_REGISTRY is required}/${SERVICE_IMAGE_REPO_OWNER:?SERVICE_IMAGE_REPO_OWNER is required}/${SERVICE_IMAGE_NAME:?SERVICE_IMAGE_NAME is required}"
image_tag="${GITHUB_SHA:?GITHUB_SHA is required}"
image_ref="${image_repo}:${image_tag}"

# 除了不可变的 commit SHA tag, 当这次构建是打在一个 git tag 上时(跨仓日快照
# uat-daily-build-*, 或 release v*), 额外推一个同名的镜像 tag。
#
# gitops 的 compose/<domain>/.env.<env> 是按"跨仓日快照 tag"把同一轮的多个服务
# 钉在一起的(accounts / billing-service / console 都产出这个 tag)。bridge 以前
# 只推 SHA tag, 所以 platform-ops 的自动改 tag 会把 XWORKMATE_BRIDGE_IMAGE 写成
# GHCR 里根本不存在的 uat-daily-build-*, 而 compose 是整体先 pull 再启动 ——
# 一个镜像拉不到, 整个 web-saas stack 都起不来(2026-08-14 的 UAT 事故)。
image_refs="${image_ref}"
if [[ "${GITHUB_REF_TYPE:-}" == "tag" && -n "${GITHUB_REF_NAME:-}" ]]; then
  image_refs="${image_refs}"$'\n'"${image_repo}:${GITHUB_REF_NAME}"
fi

printf 'image_repo=%s\n' "${image_repo}" >> "${GITHUB_OUTPUT}"
printf 'image_tag=%s\n' "${image_tag}" >> "${GITHUB_OUTPUT}"
printf 'image_ref=%s\n' "${image_ref}" >> "${GITHUB_OUTPUT}"

# build-push-action 的 tags 接受换行分隔的多个 ref; 多行输出必须走 heredoc 语法。
{
  printf 'image_refs<<__IMAGE_REFS_EOF__\n'
  printf '%s\n' "${image_refs}"
  printf '__IMAGE_REFS_EOF__\n'
} >> "${GITHUB_OUTPUT}"
