#!/usr/bin/env python3
"""Zero-Leak Antigravity Gateway for Cloud Code Google Frontend.
Guarantees 100% complete JSON-structure-aware sanitization of any third-party coding
agent and reverse-proxy signals, ensuring Google upstream sees only authentic
native Antigravity traffic without corrupting model IDs, signatures, or JSON syntax.
"""
import http.client
import http.server
import json
import logging
import os
import re
import socket
import ssl
import time
from typing import Any

FRONTENDS = (
    'oauth2.googleapis.com',
    'www.google.com',
    'accounts.google.com',
    'api.googleapis.com',
)

DEFAULT_ANTIGRAVITY_UA = 'antigravity/hub/2.33.0 (aidev_client; os_type=darwin; arch=arm64)'

# 仅当值类型为字符串时才跳过的受保护字段；若其值是 dict/list（如 properties.name/model/type 对象）则继续向下递归清洗
PROTECTED_STRING_KEYS = frozenset({
    'model',
    'project',
    'requestId',
    'sessionId',
    'userAgent',
    'thoughtSignature',
    'signature',
    'mimeType',
    'role',
    'type',
    'id',
    'callId',
    'name',
})

# 需跳过整棵子树的字段（保护 base64 媒体内容与 JSON Schema required 字段名列表）
PROTECTED_SUBTREE_KEYS = frozenset({
    'inlineData',
    'fileData',
    'required',
})

# 按“先 WAF 429 与越狱拦截、长句/具体域名与系统标签、后短词”有序排列的正则替换规则（仅用于字符串值清洗）
TEXT_REPLACEMENTS: list[tuple[re.Pattern[str], str]] = [
    (re.compile(r'(?i)\b(?:bypass|bypasses|bypassing)\b'), 'troubleshoot'),
    # 0. 高危越狱/红队与社区实锤 Antigravity 裸 429 WAF 触发特征清理（最高优先级）
    (re.compile(r'(?im)^[^\n]*\b(?:godmode|parseltongue|ultraplinian|g0dm0d3|jailbreak\s+llms?)\b[^\n]*\n?'), ''),
    (re.compile(r'(?i)\b(?:godmode|parseltongue|ultraplinian|g0dm0d3)\b'), 'redacted'),
    (re.compile(r'(?i)\bjailbreak(?:\s+llms?)?\b'), 'security-test'),
    (re.compile(r'(?i)For help with (?:Hermes Agent|Antigravity) itself\s*[—-][^\n]*(?:web content\)|invent workarounds)\.?.*'), ''),
    (re.compile(r'(?m)^#\s*Google model operational directives\s*$'), '# Operational directives'),
    (re.compile(r'(?i)<available_skills>'), 'Available capabilities:'),
    (re.compile(r'(?i)</available_skills>'), ''),
    (re.compile(r'(?m)^#\s*Tool-use enforcement\s*$'), '# Action guidelines'),
    (
        re.compile(
            r'(?is)#\s*Finishing the job\s*\n.*?(?=\n#|\Z)'
        ),
        '# Completion guidelines\nFocus on delivering working results and reporting execution output accurately.\n\n',
    ),
    (
        re.compile(
            r'(?is)When the user asks you to build, run, or verify something, the deliverable is a working artifact backed by real tool output.*?(?=\n\n|\n#|\Z)'
        ),
        'Focus on delivering working results and reporting execution output accurately.',
    ),
    (
        re.compile(
            r'(?is)If a tool, install, or network call fails and blocks the real path, say so directly and try an alternative.*?(?=\n\n|\n#|\Z)'
        ),
        'If an operation fails, report the issue directly and try an appropriate alternative.',
    ),
    (re.compile(r'(?m)^#\s*Parallel tool calls\s*$'), '# Batching tool calls'),
    (re.compile(r'(?im)^Active (?:Hermes|Antigravity) profile:[^\n]*\n?'), ''),
    (re.compile(r'(?im)^#\s*(?:Hermes|Antigravity) runtime environment\s*$'), '# Runtime environment'),
    (re.compile(r'(?i)<!--\s*End (?:Hermes|Antigravity) runtime environment\s*-->\n?'), ''),
    (re.compile(r'(?m)^═{10,}\n(?:MEMORY \(your personal notes\)|USER PROFILE \(who the user is\))[^\n]*\n═{10,}'), '## Context Notes'),
    (re.compile(r'(?i)\bRFC\s*2119\b'), 'standard'),
    (re.compile(r'(?i)from\s+(?:hermes|antigravity)_tools\s+import'), 'from local_tools import'),
    (re.compile(r'(?i)Google/OAuth/Cloud\s*Code'), 'upstream-endpoint'),
    (re.compile(r'(?i)\bmax-retry-credentials\b'), 'retry-limit'),
    (re.compile(r'(?i)\bEdgeCrab\b'), 'local-runner'),
    (re.compile(r'(?i)~/.config/JetBrains/Air'), '~/.config/desktop-ide'),
    (re.compile(r'(?i)\bJetBrains[-_ ]Air\b'), 'desktop-ide'),
    (re.compile(r'(?i)User explicitly approved "approve always" on high-risk[^\n]*'), ''),

    # 1. 清除第三方 Agent/Harness 注入的 XML/标签指令块
    (re.compile(r'(?s)<system[-_](conventions|directive)>.*?</system[-_](conventions|directive)>\n?'), ''),
    (re.compile(r'(?i)</?system-reminder>'), ''),
    (re.compile(r'(?i)</?system-update>'), ''),

    # 2. 完整身份句与帮助引导句改写为原生 Antigravity 身份
    (
        re.compile(
            r"(?is)You run on Hermes Agent \(by Nous Research\)\..*?so you don'?t guess or invent workarounds\.?"
        ),
        'You are running inside Antigravity, an AI coding assistant developed by Google.',
    ),
    (
        re.compile(r'(?i)You run on Hermes Agent[^\n.]*\.?'),
        'You are running inside Antigravity.',
    ),
    (
        re.compile(r'(?i)You are Hermes Agent,\s*built by Nous Research\.?'),
        'You are Antigravity, an advanced AI coding assistant developed by Google.',
    ),
    (
        re.compile(r'(?i)You are an AI agent running in [^\n.]*,?\s*a coding agent harness\.?'),
        'You are Antigravity, an advanced AI coding assistant developed by Google.',
    ),
    (
        re.compile(
            r'(?i)You are (?:opencode|OpenCode|OpenClaw|Claude|Codex|Cline|Cursor|Windsurf|Roo[-_ ]Code|Aider|Hermes|Kilo[-_ ]?Code|Trae|Qoder)[^\n.]*\.?'
        ),
        'You are Antigravity, an advanced AI coding assistant developed by Google.',
    ),
    (
        re.compile(r'(?i)You are (?:an? )?(?:interactive )?(?:CLI|coding) agent[^\n.]*\.?'),
        'You are Antigravity, an advanced AI coding assistant developed by Google.',
    ),

    # 3. 文档 URL、本机路径与反代/代理基础设施泄露词清理
    (re.compile(r'(?i)https?://hermes-agent\.nousresearch\.com[^\s"\']*'), 'https://antigravity.google/docs'),
    (re.compile(r'(?i)https?://toten\.940817\.xyz[^\s"\']*'), 'https://generativelanguage.googleapis.com'),
    (re.compile(r'(?i)toten\.940817\.xyz'), 'local-endpoint'),
    (re.compile(r'/root/\.hermes[^\s"\']*'), '/workspace/.assistant'),
    (re.compile(r'http://127\.0\.0\.1:(?:8090|28100|2334|2336)[^\s"\']*'), 'http://127.0.0.1/local-service'),
    (re.compile(r'127\.0\.0\.1:(?:8090|28100|2334|2336)'), 'local-service'),

    # 4. Harness / 架构模式与反代系统词
    (re.compile(r'(?i)a coding agent harness'), 'an AI coding assistant'),
    (re.compile(r'(?i)coding agent harness'), 'AI coding assistant'),
    (re.compile(r'(?m)^#\s*Harness\s*$'), '# Guidelines'),
    (re.compile(r'(?i)harness instructions'), 'assistant instructions'),
    (re.compile(r'(?i)CLIProxyAPI|CLI\s*Proxy\s*API'), 'local gateway'),
    (re.compile(r'(?i)CLIProxy|CLI\s*Proxy'), 'local gateway'),
    (re.compile(r'(?i)\bsub2api\b'), 'local service'),
    (re.compile(r'(?i)\bOpenResty\b'), 'local proxy'),
    (re.compile(r'(?i)\bSkyVPN\b'), 'network proxy'),
    (re.compile(r'(?i)\bClash\b'), 'local proxy'),
    (re.compile(r'连坐全量封禁'), '连续重试失败'),
    (re.compile(r'账号池被上游'), '服务请求异常'),

    # 5. 第三方 Agent 名称与厂商名清理（仅作用于已过滤的文本值，绝不改顶层 model）
    (re.compile(r'(?i)hermes[-_ ]agent'), 'Antigravity'),
    (re.compile(r'(?i)hermes[-_ ]gateway'), 'Antigravity'),
    (re.compile(r'(?i)\bHermes\b|hermes'), 'Antigravity'),
    (re.compile(r'(?i)nousresearch(?:\.com)?'), 'Google'),
    (re.compile(r'(?i)\bNous[-_ ]?Research\b'), 'Google'),
    (re.compile(r'(?i)\bNous\b'), 'Google'),
    (re.compile(r'(?i)opencode'), 'Antigravity'),
    (re.compile(r'(?i)openclaw'), 'Antigravity'),
    (re.compile(r'(?i)\bClaude[-_ ]Code\b'), 'Antigravity'),
    (re.compile(r'(?i)\bClaude[-_ ]Agent\b'), 'Antigravity'),
    (re.compile(r'(?i)\bOpenAI[-_ ]Codex\b'), 'Antigravity'),
    (re.compile(r'(?i)\bCodex[-_ ]CLI\b'), 'Antigravity'),
    (re.compile(r'(?i)\bCodex\b'), 'Antigravity'),
    (re.compile(r'(?i)\bGitHub[-_ ]Copilot\b'), 'Antigravity'),
    (re.compile(r'(?i)\bCopilot\b'), 'Antigravity'),
    (re.compile(r'(?i)\bJetBrains[-_ ]AI(?:[-_ ]Assistant)?\b'), 'Antigravity'),
    (re.compile(r'(?i)\bRoo[-_ ]Code\b'), 'Antigravity'),
    (re.compile(r'(?i)\bKilo[-_ ]?Code\b'), 'Antigravity'),
    (re.compile(r'(?i)\bContinue\.dev\b'), 'Antigravity'),
    (re.compile(r'(?i)\bCursor\b'), 'Antigravity'),
    (re.compile(r'(?i)\bWindsurf\b'), 'Antigravity'),
    (re.compile(r'(?i)\bCline\b'), 'Antigravity'),
    (re.compile(r'(?i)\bAider\b'), 'Antigravity'),
    (re.compile(r'(?i)\bTrae\b'), 'Antigravity'),
    (re.compile(r'(?i)\bQoder\b'), 'Antigravity'),
    (re.compile(r'(?i)\bDevin\b'), 'Antigravity'),
    (re.compile(r'(?i)\bAnthropic\b'), 'Google'),
    (re.compile(r'(?i)\bClaude\b'), 'Antigravity'),
    (re.compile(r'\bHttpX\b'), 'httpx'),
]

# 非 JSON 回退时的安全字节规则（绝不替换 "model" 值，且不含裸换行符）
FALLBACK_BYTE_REPLACEMENTS: list[tuple[re.Pattern[bytes], bytes]] = [
    (
        re.compile(rb'(?i)You are (?:opencode|Hermes|Claude|Codex|Cline)[^\n."\']*'),
        b'You are Antigravity, an advanced AI coding assistant developed by Google.',
    ),
    (re.compile(rb'(?i)opencode'), b'Antigravity'),
    (re.compile(rb'(?i)openclaw'), b'Antigravity'),
    (re.compile(rb'(?i)hermes[-_ ]?(?:agent|gateway)?'), b'Antigravity'),
    (re.compile(rb'(?i)nous\s*research|nousresearch'), b'Google'),
    (re.compile(rb'(?i)cliproxy(?:api)?|cli\s*proxy(?:\s*api)?'), b'local gateway'),
    (re.compile(rb'(?i)sub2api'), b'local service'),
    (re.compile(rb'(?i)openresty'), b'local proxy'),
    (re.compile(rb'(?i)skyvpn'), b'network proxy'),
    (re.compile(rb'(?i)claude[-_ ](?:code|agent)'), b'Antigravity'),
    (re.compile(rb'(?i)\bAnthropic\b'), b'Google'),
    (re.compile(rb'(?i)coding agent harness'), b'AI coding assistant'),
    (re.compile(rb'(?i)</?system-reminder>|</?system-update>'), b''),
    (re.compile(rb'(?i)\b(?:godmode|parseltongue|ultraplinian|g0dm0d3|jailbreak)\b'), b'redacted'),
    (re.compile(rb'(?i)<available_skills>'), b'Available capabilities:'),
    (re.compile(rb'(?i)</available_skills>'), b''),
    (
        re.compile(
            rb'(?is)When the user asks you to build, run, or verify something, the deliverable is a working artifact backed by real tool output.*?(?=\n\n|\n#|\Z)'
        ),
        b'Focus on delivering working results and reporting execution output accurately.',
    ),
    (
        re.compile(
            rb'(?is)If a tool, install, or network call fails and blocks the real path, say so directly and try an alternative.*?(?=\n\n|\n#|\Z)'
        ),
        b'If an operation fails, report the issue directly and try an appropriate alternative.',
    ),
]

# 出境审计黑名单正则：排除顶层 model 字段后，任意命中均视为泄露风险或高危 WAF 429 触发词
LEAK_AUDIT_RE = re.compile(
    r'(?i)(hermes|nousresearch|nous\s*research|cliproxy|cli\s*proxy|sub2api|openresty|skyvpn|'
    r'opencode|openclaw|claude[-_\s]*(?:code|agent)|coding\s*agent\s*harness|'
    r'</?system-reminder>|</?system-update>|连坐|全量封禁|toten\.940817|'
    r'jailbreak|godmode|parseltongue|ultraplinian|#\s*Google model operational directives|'
    r'</?available_skills>|For help with|authoritative reference|RFC\s*2119|'
    r'max-retry-credentials|EdgeCrab|direct bypass|Antigravityr|'
    r'deliverable is a working artifact|fabricated output)'
)


RULES_CONFIG_FILE = '/usr/local/etc/cloudcode-rules.json'

class DynamicRuleEngine:
    def __init__(self, config_path: str = RULES_CONFIG_FILE):
        self.config_path = config_path
        self._last_mtime = 0.0
        self._last_check = 0.0
        self.harness_strip_rules: list[tuple[re.Pattern[str], str]] = []
        self.custom_text_rules: list[tuple[re.Pattern[str], str]] = []
        self.custom_leak_terms: set[str] = set()
        self.reload_if_needed(force=True)

    def reload_if_needed(self, force: bool = False) -> None:
        now = time.monotonic()
        if not force and (now - self._last_check < 3.0):
            return
        self._last_check = now

        try:
            if not os.path.exists(self.config_path):
                return
            mtime = os.path.getmtime(self.config_path)
            if mtime <= self._last_mtime and not force:
                return

            with open(self.config_path, 'r', encoding='utf-8') as f:
                data = json.load(f)

            new_strip = []
            for item in data.get('harness_strip_sections', []):
                new_strip.append((re.compile(item['pattern']), item['replacement']))

            new_text = []
            for item in data.get('custom_replacements', []):
                new_text.append((re.compile(item['pattern']), item['replacement']))

            self.harness_strip_rules = new_strip
            self.custom_text_rules = new_text
            self.custom_leak_terms = set(data.get('leak_audit_terms', []))
            self._last_mtime = mtime
            logging.info('cloudcode-rules dynamic reload success: %d strip rules, %d custom rules from %s',
                         len(new_strip), len(new_text), self.config_path)
        except Exception as e:
            logging.warning('cloudcode-rules dynamic reload failed: %s', e)

    def sanitize(self, value: str) -> str:
        self.reload_if_needed()
        updated = value
        # 1. 优先执行整块 Harness 模板剥离与指纹中和 (Dimension 1)
        for pat, rep in self.harness_strip_rules:
            updated = pat.sub(rep, updated)
        # 2. 执行静态内置核心规则
        for pat, rep in TEXT_REPLACEMENTS:
            updated = pat.sub(rep, updated)
        # 3. 执行动态自定义扩展规则 (Dimension 2)
        for pat, rep in self.custom_text_rules:
            updated = pat.sub(rep, updated)
        return updated

RULE_ENGINE = DynamicRuleEngine()


def _sanitize_text(value: str) -> str:
    return RULE_ENGINE.sanitize(value)


def _sanitize_json_node(node: Any) -> Any:
    if isinstance(node, str):
        return _sanitize_text(node)
    if isinstance(node, list):
        return [_sanitize_json_node(item) for item in node]
    if isinstance(node, dict):
        is_thought_part = node.get('thought') is True
        result: dict[str, Any] = {}
        for k, v in node.items():
            if k in PROTECTED_SUBTREE_KEYS or (is_thought_part and k == 'text') or (
                k in PROTECTED_STRING_KEYS and isinstance(v, str)
            ):
                result[k] = v
            else:
                result[k] = _sanitize_json_node(v)
        return result
    return node


def _collect_audit_strings(node: Any, out: list[str]) -> None:
    if isinstance(node, str):
        out.append(node)
    elif isinstance(node, list):
        for item in node:
            _collect_audit_strings(item, out)
    elif isinstance(node, dict):
        is_thought_part = node.get('thought') is True
        for k, v in node.items():
            if (
                k in PROTECTED_SUBTREE_KEYS
                or (is_thought_part and k == 'text')
                or (k in ('model', 'thoughtSignature', 'signature') and isinstance(v, str))
            ):
                continue
            _collect_audit_strings(v, out)


def audit_payload_leaks(data: bytes) -> tuple[list[str], str]:
    """检查清洗后字节流中是否残留敏感关键词（排除 model 等受保护字段），返回 (命中词集合, model名)。"""
    model_name = '-'
    try:
        parsed = json.loads(data.decode('utf-8'))
        if isinstance(parsed, dict) and isinstance(parsed.get('model'), str):
            model_name = parsed['model']
        strings: list[str] = []
        _collect_audit_strings(parsed, strings)
        haystack = '\n'.join(strings)
    except Exception:
        haystack = data.decode('utf-8', errors='replace')
    hits_set = {m.group(0).lower() for m in LEAK_AUDIT_RE.finditer(haystack)}
    for term in RULE_ENGINE.custom_leak_terms:
        try:
            if re.search(r'(?i)' + term, haystack):
                hits_set.add(term.lower())
        except re.error:
            pass
    hits = sorted(hits_set)
    return hits, model_name



def sanitize_gemini_schema(node: dict) -> dict:
    if not isinstance(node, dict):
        return node

    result = {}
    for k, v in node.items():
        if k in ("$schema", "$ref", "definitions"):
            continue
        elif k == "const":
            result["enum"] = [v]
        elif k in ("anyOf", "oneOf", "allOf"):
            branches = v if isinstance(v, list) else []
            if branches:
                first = branches[0]
                if isinstance(first, dict):
                    sanitized_first = sanitize_gemini_schema(first)
                    for fk, fv in sanitized_first.items():
                        if fk not in result:
                            result[fk] = fv
                    extra_desc = f" (Accepts {len(branches)} alternative forms)"
                    existing_desc = result.get("description", "")
                    if extra_desc not in existing_desc:
                        result["description"] = (existing_desc + extra_desc).strip()
                else:
                    result["type"] = "string"
            else:
                result["type"] = "string"
        elif isinstance(v, dict):
            result[k] = sanitize_gemini_schema(v)
        elif isinstance(v, list):
            result[k] = [sanitize_gemini_schema(item) if isinstance(item, dict) else item for item in v]
        else:
            result[k] = v

    return result

def sanitize_antigravity_tools(payload_obj: dict) -> int:
    cleaned_count = 0
    candidates = []
    if "request" in payload_obj and isinstance(payload_obj["request"], dict):
        if "tools" in payload_obj["request"]:
            candidates.append(payload_obj["request"]["tools"])
    if "tools" in payload_obj:
        candidates.append(payload_obj["tools"])

    for tools_list in candidates:
        if isinstance(tools_list, list):
            for t in tools_list:
                if isinstance(t, dict) and "functionDeclarations" in t:
                    fds = t.get("functionDeclarations", [])
                    if isinstance(fds, list):
                        for fd in fds:
                            if isinstance(fd, dict) and "parameters" in fd and isinstance(fd["parameters"], dict):
                                orig_str = json.dumps(fd["parameters"])
                                fd["parameters"] = sanitize_gemini_schema(fd["parameters"])
                                new_str = json.dumps(fd["parameters"])
                                if orig_str != new_str:
                                    cleaned_count += 1
    return cleaned_count


def sanitize_payload(data: bytes) -> tuple[bytes, int]:
    """对发往 Google 上游的请求载荷执行 JSON 结构感知脱敏与出境审计，返回 (脱敏字节, 泄露命中数)。"""
    if not data:
        return data, 0

    try:
        parsed = json.loads(data.decode('utf-8'))
        cleaned = _sanitize_json_node(parsed)
        tools_cleaned = sanitize_antigravity_tools(cleaned)
        if tools_cleaned > 0:
            logging.info("tools_schema_sanitized count=%d", tools_cleaned)
        sanitized = json.dumps(cleaned, ensure_ascii=False, separators=(',', ':')).encode('utf-8')
    except Exception:
        sanitized = data
        for pat, rep in FALLBACK_BYTE_REPLACEMENTS:
            sanitized = pat.sub(rep, sanitized)

    hits, model_name = audit_payload_leaks(sanitized)
    if hits:
        logging.warning('sanitize_audit model=%s leak_hits=%d terms=%s', model_name, len(hits), ','.join(hits))
    return sanitized, len(hits)


class GoogleConnection(http.client.HTTPSConnection):
    def __init__(self, frontend: str, **kwargs: Any):
        self.frontend = frontend
        super().__init__(frontend, **kwargs)

    def connect(self) -> None:
        s = socket.create_connection(('127.0.0.1', 2334), timeout=15)
        target = (self.frontend + ':443').encode()
        s.sendall(b'CONNECT ' + target + b' HTTP/1.1\r\nHost: ' + target + b'\r\n\r\n')
        h = b''
        while not h.endswith(b'\r\n\r\n'):
            x = s.recv(1)
            if not x or len(h) > 32768:
                raise OSError('SkyVPN proxy handshake failed')
            h += x
        if h.split(b' ', 2)[1] != b'200':
            raise OSError('SkyVPN proxy refused connection')
        self.sock = ssl.create_default_context().wrap_socket(s, server_hostname=self.frontend)
        self.sock.settimeout(180)


def resolve_route(method: str, path: str) -> tuple[str, str] | None:
    if method == 'GET' and path.split('?', 1)[0] == '/oauth-userinfo':
        return ('openidconnect.googleapis.com', '/v1/userinfo')
    if path.startswith('/v1internal:'):
        return ('daily-cloudcode-pa.googleapis.com', path)
    if method == 'POST' and path.startswith('/cloudcode-management/'):
        parts = path.split('/')
        if (
            len(parts) == 4
            and parts[2]
            in (
                'daily-cloudcode-pa.googleapis.com',
                'daily-cloudcode-pa.sandbox.googleapis.com',
                'cloudcode-pa.googleapis.com',
            )
            and parts[3]
            in (
                'v1internal:fetchAvailableModels',
                'v1internal:retrieveUserQuotaSummary',
                'v1internal:loadCodeAssist',
            )
        ):
            return (parts[2], '/' + parts[3])
    return None


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def log_message(self, *args: Any) -> None:
        pass

    def do_POST(self) -> None:
        self.forward()

    def do_GET(self) -> None:
        self.forward()

    def forward(self) -> None:
        route = resolve_route(self.command, self.path)
        if route is None:
            self.send_error(404)
            return
        target_host, target_path = route

        try:
            n = int(self.headers.get('Content-Length', '0'))
        except ValueError:
            self.send_error(400)
            return

        if n > 16777216:
            self.send_error(413)
            return

        body = self.rfile.read(n) if n else None
        leak_hits = 0
        model_name = '-'
        if body:
            body, leak_hits = sanitize_payload(body)
            _, model_name = audit_payload_leaks(body)

        inbound_ua = (self.headers.get('User-Agent') or '').strip()
        # 新版 CLIProxy 发送 antigravity/hub/2.33.0 时保持原样，不降级覆盖
        outbound_ua = inbound_ua if inbound_ua.lower().startswith('antigravity/') else DEFAULT_ANTIGRAVITY_UA

        headers = {
            k: v
            for k, v in self.headers.items()
            if k.lower()
            not in (
                'host',
                'connection',
                'transfer-encoding',
                'content-length',
                'proxy-authorization',
                'accept-encoding',
                'user-agent',
            )
        }
        headers['Host'] = target_host
        headers['Connection'] = 'close'
        headers['User-Agent'] = outbound_ua
        if body is not None:
            headers['Content-Length'] = str(len(body))

        body_bytes = len(body) if body is not None else 0

        import os.path
        if os.path.exists('/tmp/offline_intercept.flag'):
            try:
                if body:
                    with open('/tmp/last_intercepted_payload.json', 'wb') as f_dump:
                        f_dump.write(body)
                with open('/tmp/last_intercepted_meta.json', 'w', encoding='utf-8') as f_meta:
                    safe_headers = {k: ('***' if k.lower() in ('authorization', 'x-goog-api-key') else v) for k, v in headers.items()}
                    json.dump({'path': target_path, 'target_host': target_host, 'model': model_name, 'headers': safe_headers}, f_meta, indent=2)
            except Exception as dump_e:
                logging.warning('offline intercept dump error: %s', dump_e)
            
            logging.info('OFFLINE_INTERCEPTED: route=%s model=%s bytes=%d', target_path, model_name, body_bytes)
            mock_payload = b'data: {"candidates": [{"content": {"parts": [{"text": "OFFLINE_AUDIT_OK"}]}}]}' + b'\n\ndata: [DONE]\n\n'
            self.send_response(200)
            self.send_header('Content-Type', 'text/event-stream')
            self.send_header('Transfer-Encoding', 'chunked')
            self.end_headers()
            self.wfile.write(('%X\r\n' % len(mock_payload)).encode() + mock_payload + b'\r\n')
            self.wfile.write(b'0\r\n\r\n')
            self.wfile.flush()
            return

        for frontend in FRONTENDS:
            conn = None
            sent = False
            try:
                conn = GoogleConnection(frontend, timeout=180)
                conn.request(self.command, target_path, body=body, headers=headers)
                resp = conn.getresponse()
                if target_host == 'daily-cloudcode-pa.googleapis.com':
                    logging.info(
                        'egress route=%s model=%s ua=%s bytes=%d leak_hits=%d status=%d',
                        target_path,
                        model_name,
                        outbound_ua,
                        body_bytes,
                        leak_hits,
                        resp.status,
                    )
                self.send_response(resp.status)
                for k, v in resp.getheaders():
                    if k.lower() not in ('transfer-encoding', 'content-length', 'connection', 'content-encoding'):
                        self.send_header(k, v)
                self.send_header('Transfer-Encoding', 'chunked')
                self.end_headers()
                sent = True
                while True:
                    chunk = resp.read1(65536)
                    if not chunk:
                        break
                    self.wfile.write(('%X\r\n' % len(chunk)).encode() + chunk + b'\r\n')
                    self.wfile.flush()
                self.wfile.write(b'0\r\n\r\n')
                self.wfile.flush()
                return
            except (BrokenPipeError, ConnectionResetError):
                return
            except Exception as e:
                logging.warning('frontend_failover=%s error=%s', frontend, type(e).__name__)
                if sent:
                    self.close_connection = True
                    return
            finally:
                if conn:
                    conn.close()

        if target_host == 'daily-cloudcode-pa.googleapis.com':
            logging.info(
                'egress route=%s model=%s ua=%s bytes=%d leak_hits=%d status=502',
                target_path,
                model_name,
                outbound_ua,
                body_bytes,
                leak_hits,
            )
        try:
            self.send_error(502, 'Google frontend transport failed')
        except (BrokenPipeError, ConnectionResetError):
            self.close_connection = True


if __name__ == '__main__':
    logging.basicConfig(level=logging.INFO, format='%(asctime)s %(message)s')
    http.server.ThreadingHTTPServer(('127.0.0.1', 28100), Handler).serve_forever()
