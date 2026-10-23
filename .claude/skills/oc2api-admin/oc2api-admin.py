#!/usr/bin/env python3
"""
oc2api-admin — opencode2api 实例管理脚本

用法:
  python3 .claude/skills/oc2api-admin/oc2api-admin.py [选项] <子命令> [参数]

选项:
  -u, --url URL          实例地址 (默认 http://localhost:8080)
  -p, --password PASS    管理密码 (默认空)
  -j, --json             以 JSON 格式输出

子命令:
  probe                  探测实例是否在线
  config [get]           读取运行时配置
  config set KEY=VAL ... 修改运行时配置（支持 JSON 值）
  reload                 刷新 OpenCode 会话和模型列表
  stats [get]            查看 token 用量统计
  stats clear            清除统计数据

示例:
  python3 .claude/skills/oc2api-admin/oc2api-admin.py -u http://localhost:8001 -p change-me probe
  python3 .claude/skills/oc2api-admin/oc2api-admin.py -u http://localhost:8001 -p change-me config get
  python3 .claude/skills/oc2api-admin/oc2api-admin.py -u http://localhost:8001 -p change-me config set force_free=true
  python3 .claude/skills/oc2api-admin/oc2api-admin.py -u http://localhost:8001 -p change-me reload
  python3 .claude/skills/oc2api-admin/oc2api-admin.py -u http://localhost:8001 -p change-me stats
  python3 .claude/skills/oc2api-admin/oc2api-admin.py -u http://localhost:8001 -p change-me stats clear
"""

import argparse
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request

# /api/config 里由服务端运行时状态派生的只读字段，回写时必须剔除：
# 它们不属于 AppConfig，带上会被忽略，留着只会污染请求体。
READ_ONLY_CONFIG_KEYS = frozenset({"key_pool_status", "oc_session_map_size"})

# AppConfig 里存在、但 GET /api/config 不下发的字段。合并回写无法保住它们的
# 原有取值（客户端读不到），只能靠直接改配置文件。这里仅用于提示用户。
UNEXPOSED_CONFIG_KEYS = frozenset({
    "native_responses_models",
    "stream_empty_retry_max",
    "stream_first_byte_timeout_ms",
})


class AdminClient:
    """opencode2api 管理 API 客户端。

    认证流程：
      1. POST /login (form: password=…) → 获取 session cookie 和 csrf_token cookie
      2. 后续请求带上 Cookie header（session + csrf_token）
      3. 非 GET 请求额外附加 X-CSRF-Token header（值与 csrf_token cookie 相同）
    """

    def __init__(self, base_url: str, password: str):
        self.base_url = base_url.rstrip("/")
        self.password = password
        self._session = ""
        self._csrf = ""

    def _login(self):
        """登录，获取 session 和 CSRF token。"""
        if not self.password:
            return  # 无密码模式：无需认证

        body = urllib.parse.urlencode({"password": self.password}).encode()
        req = urllib.request.Request(
            f"{self.base_url}/login",
            data=body,
            headers={"Content-Type": "application/x-www-form-urlencoded"},
            method="POST",
        )
        # 禁止跟随重定向以捕获 Set-Cookie
        opener = urllib.request.build_opener(
            urllib.request.HTTPCookieProcessor(None)
        )
        # 手动处理重定向
        class NoRedirect(urllib.request.HTTPErrorProcessor):
            def http_response(self, request, response):
                return response
            https_response = http_response

        opener = urllib.request.build_opener(NoRedirect)
        try:
            resp = opener.open(req)
        except urllib.error.HTTPError as e:
            resp = e

        cookies = resp.headers.get_all("Set-Cookie") or []
        for c in cookies:
            name, _, rest = c.partition("=")
            value = rest.split(";")[0]
            if name.strip() == "session":
                self._session = value.strip()
            elif name.strip() == "csrf_token":
                self._csrf = value.strip()

        if not self._session:
            raise RuntimeError(
                f"登录失败（HTTP {resp.status}）：密码错误或实例未开启管理面板"
            )

    def _cookie_header(self) -> str:
        parts = []
        if self._session:
            parts.append(f"session={self._session}")
        if self._csrf:
            parts.append(f"csrf_token={self._csrf}")
        return "; ".join(parts)

    def _request(self, method: str, path: str, payload=None) -> dict:
        """发送带认证的 API 请求，返回解析后的 JSON。"""
        if not self._session and self.password:
            self._login()

        url = f"{self.base_url}{path}"
        body = json.dumps(payload).encode() if payload is not None else None
        headers = {}

        if self._cookie_header():
            headers["Cookie"] = self._cookie_header()
        if method != "GET" and self._csrf:
            headers["X-CSRF-Token"] = self._csrf
        if body:
            headers["Content-Type"] = "application/json"
            headers["Content-Length"] = str(len(body))
        elif method in ("POST", "DELETE") and body is None:
            headers["Content-Length"] = "0"

        req = urllib.request.Request(url, data=body, headers=headers, method=method)
        try:
            with urllib.request.urlopen(req) as resp:
                return json.loads(resp.read().decode())
        except urllib.error.HTTPError as e:
            err_body = e.read().decode()
            try:
                err_json = json.loads(err_body)
                raise RuntimeError(f"HTTP {e.code}: {err_json.get('error', err_body)}")
            except (ValueError, KeyError):
                raise RuntimeError(f"HTTP {e.code}: {err_body}")
        except urllib.error.URLError as e:
            raise RuntimeError(f"连接失败: {e.reason}")

    # ── 公开 API ────────────────────────────────────────────────────────────

    def probe(self) -> dict:
        """探测实例健康状态（不需要认证）。"""
        url = f"{self.base_url}/health"
        try:
            with urllib.request.urlopen(url, timeout=10) as resp:
                body = resp.read().decode()
                return {"online": True, "status": resp.status, "body": body.strip()}
        except urllib.error.URLError as e:
            return {"online": False, "error": str(e.reason)}

    def config_get(self) -> dict:
        return self._request("GET", "/api/config")

    def config_set(self, updates: dict) -> dict:
        """增量修改配置：先取完整运行时配置，合并更新后整份回写。

        服务端 POST /api/config 会用提交内容**整体覆盖**配置文件并全量应用，
        只提交变更字段会把其余字段清成零值——其中 active_socks5 变空字符串
        等于改为直连，key_pool / protocol_rules 等则直接从磁盘消失，重启即丢。
        所以这里必须先 GET 拿全量快照再合并，不能只 POST 增量。
        """
        if not updates:
            raise RuntimeError("没有需要修改的配置项")

        snapshot = self.config_get()
        # 兼容将来可能出现的 {"config": {...}} 包装。
        if isinstance(snapshot.get("config"), dict):
            snapshot = snapshot["config"]
        if not isinstance(snapshot, dict) or not snapshot:
            raise RuntimeError("读取现有配置失败，已中止写入（避免覆盖掉其余配置）")

        base = {k: v for k, v in snapshot.items() if k not in READ_ONLY_CONFIG_KEYS}
        lost = sorted(UNEXPOSED_CONFIG_KEYS - set(snapshot))
        if lost and os.environ.get("OC2API_QUIET") != "1":
            print(
                f"提示: 服务端未下发 {', '.join(lost)}，回写会将其恢复为默认值；"
                f"如需保留请直接编辑宿主机配置文件。",
                file=sys.stderr,
            )

        return self._request("POST", "/api/config", {**base, **updates})

    def reload(self) -> dict:
        return self._request("POST", "/api/reload")

    def stats_get(self) -> dict:
        return self._request("GET", "/api/stats")

    def stats_clear(self) -> dict:
        return self._request("DELETE", "/api/stats")


# ── CLI 格式化输出 ───────────────────────────────────────────────────────────

def _fmt_probe(data: dict) -> str:
    if data["online"]:
        return f"✓ 在线 (HTTP {data['status']})"
    return f"✗ 离线: {data.get('error', '未知错误')}"


def _fmt_config(data: dict) -> str:
    lines = []
    key_order = [
        "upstream_base_urls", "force_free", "force_disable_thinking",
        "max_tokens_cap", "max_tokens_cap_per_model", "model_alias",
        "protocol_rules", "key_pool", "key_pool_status",
        "socks5_proxies", "active_socks5", "socks5_paid_direct", "socks5_sticky",
        "prompt_cache_retention", "cache_control_breakpoints", "text_only_models",
        "reasoning_effort_map", "log_level", "log_bodies",
        "oc_session_map_size",
    ]
    shown = set()
    for k in key_order:
        if k in data:
            shown.add(k)
            v = data[k]
            if isinstance(v, (dict, list)) and v:
                lines.append(f"  {k}: {json.dumps(v, ensure_ascii=False)}")
            elif isinstance(v, (dict, list)):
                lines.append(f"  {k}: (空)")
            else:
                lines.append(f"  {k}: {v}")
    for k, v in data.items():
        if k not in shown:
            lines.append(f"  {k}: {json.dumps(v, ensure_ascii=False) if isinstance(v, (dict, list)) else v}")
    return "\n".join(lines)


def _fmt_reload(data: dict) -> str:
    return (
        f"会话: {data.get('session', '-')}  "
        f"免费模型: {data.get('free', '-')}  "
        f"Go 模型: {data.get('go', '-')}"
    )


def _fmt_stats(data: dict) -> str:
    lines = []
    totals: dict[str, int] = {}
    for model, s in sorted(data.items()):
        if not isinstance(s, dict):
            continue
        lines.append(
            f"  {model}: req={s.get('request_count',0)}"
            f"  prompt={s.get('prompt_tokens',0)}"
            f"  completion={s.get('completion_tokens',0)}"
            f"  total={s.get('total_tokens',0)}"
            f"  cache_read={s.get('cache_read_tokens',0)}"
        )
        for field in ("request_count", "prompt_tokens", "completion_tokens",
                      "total_tokens", "cache_read_tokens"):
            totals[field] = totals.get(field, 0) + s.get(field, 0)
    if totals:
        lines.append(
            f"\n  合计: req={totals.get('request_count',0)}"
            f"  prompt={totals.get('prompt_tokens',0)}"
            f"  completion={totals.get('completion_tokens',0)}"
            f"  total={totals.get('total_tokens',0)}"
            f"  cache_read={totals.get('cache_read_tokens',0)}"
        )
    return "\n".join(lines) if lines else "  (无统计数据)"


# ── 参数解析 & 主流程 ─────────────────────────────────────────────────────────

def main():
    parser = argparse.ArgumentParser(
        description="opencode2api 实例管理脚本",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=__doc__,
    )
    parser.add_argument("-u", "--url", default="http://localhost:8080", help="实例地址")
    parser.add_argument("-p", "--password", default="", help="管理密码")
    parser.add_argument("-j", "--json", action="store_true", help="JSON 格式输出")

    sub = parser.add_subparsers(dest="cmd", required=True)

    sub.add_parser("probe", help="探测实例在线状态")

    cfg_p = sub.add_parser("config", help="读取或修改运行时配置")
    cfg_p.add_argument("action", nargs="?", default="get",
                       help="get（默认）或 set KEY=VAL ...")
    cfg_p.add_argument("pairs", nargs="*", metavar="KEY=VAL",
                       help="要修改的配置项（set 时使用）")

    sub.add_parser("reload", help="刷新 OpenCode 会话和模型列表")

    stats_p = sub.add_parser("stats", help="查看或清除 token 用量统计")
    stats_p.add_argument("action", nargs="?", default="get",
                         choices=["get", "clear"], help="get（默认）或 clear")

    args = parser.parse_args()

    client = AdminClient(args.url, args.password)

    try:
        if args.cmd == "probe":
            result = client.probe()
            if args.json:
                print(json.dumps(result, ensure_ascii=False, indent=2))
            else:
                print(_fmt_probe(result))

        elif args.cmd == "config":
            if args.action == "set":
                if not args.pairs:
                    parser.error("config set 需要至少一个 KEY=VAL 参数")
                updates = {}
                for pair in args.pairs:
                    if "=" not in pair:
                        parser.error(f"无法解析参数: {pair!r}（格式应为 KEY=VAL）")
                    k, _, v = pair.partition("=")
                    try:
                        updates[k] = json.loads(v)
                    except (ValueError, json.JSONDecodeError):
                        updates[k] = v  # 当作字符串
                result = client.config_set(updates)
                if args.json:
                    print(json.dumps(result, ensure_ascii=False, indent=2))
                else:
                    print(f"✓ 配置已更新: {', '.join(updates.keys())}")
            else:
                result = client.config_get()
                if args.json:
                    print(json.dumps(result, ensure_ascii=False, indent=2))
                else:
                    print("配置:")
                    print(_fmt_config(result))

        elif args.cmd == "reload":
            result = client.reload()
            if args.json:
                print(json.dumps(result, ensure_ascii=False, indent=2))
            else:
                print(f"✓ 刷新完成: {_fmt_reload(result)}")

        elif args.cmd == "stats":
            if args.action == "clear":
                result = client.stats_clear()
                if args.json:
                    print(json.dumps(result, ensure_ascii=False, indent=2))
                else:
                    print("✓ 统计数据已清除")
            else:
                result = client.stats_get()
                if args.json:
                    print(json.dumps(result, ensure_ascii=False, indent=2))
                else:
                    print("Token 用量统计:")
                    print(_fmt_stats(result))

    except RuntimeError as e:
        print(f"错误: {e}", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()
