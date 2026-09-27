#!/usr/bin/env bash
# pageshare 一键部署（Docker Compose）。
#
#   ./deploy.sh              首次部署：备 .env（自动生成随机密钥）→ 构建 → 启动 → 等健康
#   ./deploy.sh --tls        同上，附带 Caddy TLS 反代（先改 deploy/Caddyfile 的域名/邮箱）
#   ./deploy.sh stop         停止并移除容器（数据卷保留）
#   ./deploy.sh update       改完代码/镜像后滚动更新（重新 build + up）
#   ./deploy.sh status/logs  查看状态 / 跟踪日志（logs 可带服务名：./deploy.sh logs pageshare）
#   ./deploy.sh down         彻底移除（含数据卷——站点元数据会丢，慎用）
set -euo pipefail
cd "$(dirname "$0")"

ENV_FILE=.env

die() { echo "✖ $*" >&2; exit 1; }

# ---- docker / compose 方言探测 ----
have() { command -v "$1" >/dev/null 2>&1; }
docker_ok() { have docker && docker info >/dev/null 2>&1; }
compose() {
	if docker compose version >/dev/null 2>&1; then docker compose "$@"
	elif have docker-compose; then docker-compose "$@"
	else die "需要 docker compose（v2）或 docker-compose（v1）"; fi
}

rand_hex() { # 随机 hex；openssl 优先，缺则 python3，再无则 urandom
	if have openssl; then openssl rand -hex "$1"
	elif have python3; then python3 -c "import secrets;print(secrets.token_hex($1))"
	else head -c "$1" /dev/urandom | od -An -tx1 | tr -d ' \n'; fi
}

ensure_env() {
	[ -f "$ENV_FILE" ] && return 0
	echo "◆ 未发现 ${ENV_FILE}，生成默认配置（随机密钥、本地 http 模式）…"
	cat > "$ENV_FILE" <<EOF
# 由 ./deploy.sh 自动生成 $(date '+%Y-%m-%d %H:%M:%S')
# 要公网访问/泛分享子域：改 PS_PUBLIC_BASE_URL / PS_SITE_WILDCARD_HOST 后 ./deploy.sh update
PS_AUTH_TOKEN=$(rand_hex 16)
PS_COOKIE_SECRET=$(rand_hex 32)
PS_LISTEN_PORT=8300
PS_PUBLIC_BASE_URL=http://localhost:8300
PS_SITE_WILDCARD_HOST=
PS_VERSIONS_KEPT=3
PS_STORAGE_BACKEND=disk
EOF
	chmod 600 "$ENV_FILE"
	echo "◆ 已生成（管理 token 已随机化，稍后会打印）"
}

wait_healthy() {
	local port url i
	port=$(grep -E '^PS_LISTEN_PORT=' "$ENV_FILE" 2>/dev/null | cut -d= -f2 || true)
	url="http://127.0.0.1:${port:-8300}/healthz"
	for i in $(seq 1 30); do
		if have curl; then
			curl -fsS --max-time 2 "$url" >/dev/null 2>&1 && return 0
		else
			wget -q --spider --timeout=2 "$url" 2>/dev/null && return 0
		fi
		sleep 2
	done
	return 1
}

case "${1:-start}" in
	--tls)
		docker_ok || die "docker 未运行（先启动 Docker Desktop / systemctl start docker）"
		ensure_env
		echo "◆ 构建并启动（含 Caddy TLS profile）…"
		compose --profile tls up -d --build
		wait_healthy || die "60 秒内未通过健康检查：./deploy.sh logs 查看原因"
		echo
		echo "✔ 已就绪。TLS 端点以 deploy/Caddyfile 里的域名为准（记得换 email/域名）。"
		echo "  管理 token 见 ${ENV_FILE} 的 PS_AUTH_TOKEN。"
		;;
	start)
		docker_ok || die "docker 未运行（先启动 Docker Desktop / systemctl start docker）"
		ensure_env
		echo "◆ 构建镜像并启动…"
		compose up -d --build
		wait_healthy || die "60 秒内未通过健康检查：./deploy.sh logs 查看原因"
		port=$(grep -E '^PS_LISTEN_PORT=' "$ENV_FILE" | cut -d= -f2)
		token=$(grep -E '^PS_AUTH_TOKEN=' "$ENV_FILE" | cut -d= -f2)
		echo
		echo "✔ 部署完成，健康检查通过。"
		echo "   管理台: http://localhost:${port:-8300}/admin/"
		echo "   登录 Bearer token（${ENV_FILE} 里可随时改）:"
		echo "   $token"
		;;
	update)
		docker_ok || die "docker 未运行"
		compose up -d --build
		wait_healthy && echo "✔ 更新完成，健康检查通过" || die "更新后健康检查失败：./deploy.sh logs"
		;;
	stop)   compose down ;;
	down)
		echo "⚠ 将删除容器与数据卷（站点元数据/审计/暂存全部丢失）"
		read -r -p "确认？输入 yes 继续: " ans; [ "$ans" = yes ] || die "已取消"
		compose down -v ;;
	status) compose ps ;;
	logs)   compose logs -f --tail=100 "${@:2}" ;;
	*)      sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'; exit 1 ;;
esac
