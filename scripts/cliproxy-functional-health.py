#!/usr/bin/env python3
"""Dual-Track Functional & Canary Health Monitor for CLIProxy & Antigravity.
Track 1 (Standard): Light ping every 2 min to verify network egress & token validity.
Track 2 (Canary): Periodic (every 30 min or --canary) full-fidelity agent payload
                  with system prompt, tools, reasoning, and stream to proactively
                  detect upstream WAF prompt fingerprint changes before users hit them.
"""
import sys
import os
import json
import yaml
import urllib.request
import urllib.error
import time
import datetime
import subprocess
import fcntl
import pathlib

LOCK_PATH = '/run/lock/cliproxy-functional-health.lock'
STATE_PATH = pathlib.Path('/var/lib/skyvpn/service-health.json')
CONFIG_PATH = '/opt/clirelay-validation/config.production.yaml'
CANARY_LOG_PATH = '/var/log/cliproxy-canary-waf.log'
CANARY_INTERVAL_SECONDS = 1800  # Run canary every 30 minutes

lock = open(LOCK_PATH, 'w')
try:
    fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
except BlockingIOError:
    raise SystemExit(0)

try:
    old_state = json.loads(STATE_PATH.read_text())
except Exception:
    old_state = {}

try:
    cfg = yaml.safe_load(open(CONFIG_PATH))
    api_key = cfg['api-keys'][0]
except Exception as e:
    api_key = 'sk-proxy-admin-root'

BASE_URL = 'http://127.0.0.1:8090/v1/chat/completions'
HEADERS = {
    'Authorization': 'Bearer ' + api_key,
    'Content-Type': 'application/json'
}

# --- Track 1: Light Ping Payload ---
LIGHT_BODY = json.dumps({
    'model': 'gemini-3.8-flash-high',
    'messages': [{'role': 'user', 'content': '只回复 OK'}],
    'max_tokens': 512,
    'stream': False
}).encode()

# --- Track 2: Realistic Canary Agent Payload ---
CANARY_SYSTEM_PROMPT = """# Assistant Persona & Server Rules
You are running inside Antigravity, an advanced AI coding assistant developed by Google.
## Action guidelines
Batch independent tool calls when reading or searching.
# Completion guidelines
Focus on delivering working results and reporting execution output accurately.
If an operation fails, report the issue directly and try an appropriate alternative.
"""

CANARY_TOOLS = [{
    'type': 'function',
    'function': {
        'name': 'execute_code',
        'description': 'Run code snippet or command in isolated sandbox',
        'parameters': {
            'type': 'object',
            'properties': {
                'code': {'type': 'string', 'description': 'The code to execute'}
            },
            'required': ['code']
        }
    }
}]

CANARY_BODY = json.dumps({
    'model': 'gemini-3.8-flash-high',
    'messages': [
        {'role': 'system', 'content': CANARY_SYSTEM_PROMPT},
        {'role': 'user', 'content': '请仅回复：OK'}
    ],
    'reasoning_effort': 'medium',
    'max_tokens': 512,
    'stream': True,
    'tools': CANARY_TOOLS
}).encode()


def probe_light():
    t0 = time.monotonic()
    try:
        req = urllib.request.Request(BASE_URL, data=LIGHT_BODY, headers=HEADERS)
        with urllib.request.urlopen(req, timeout=45) as r:
            data = json.load(r)
        content = (data.get('choices') or [{}])[0].get('message', {}).get('content')
        ok = content is not None
        return ok, 'healthy' if ok else 'empty_response', 200, round(time.monotonic() - t0, 2)
    except urllib.error.HTTPError as e:
        body_text = e.read(2000).decode(errors='replace')
        reason = ('transport' if any(w in body_text for w in ('EOF', 'TLS', 'timeout', 'Google frontend transport failed'))
                  else 'account_verification' if 'Verify your account' in body_text
                  else 'quota' if e.code == 429
                  else f'upstream_{e.code}')
        return False, reason, e.code, round(time.monotonic() - t0, 2)
    except Exception as e:
        return False, 'transport', type(e).__name__, round(time.monotonic() - t0, 2)


def probe_canary():
    t0 = time.monotonic()
    try:
        req = urllib.request.Request(BASE_URL, data=CANARY_BODY, headers=HEADERS)
        with urllib.request.urlopen(req, timeout=45) as r:
            first_chunk = r.read(256).decode(errors='replace')
        ok = 'data:' in first_chunk or 'choices' in first_chunk
        return ok, 'canary_ok', 200, round(time.monotonic() - t0, 2), ''
    except urllib.error.HTTPError as e:
        err_msg = e.read(2000).decode(errors='replace')
        reason = 'canary_waf_429' if e.code == 429 else f'canary_upstream_{e.code}'
        return False, reason, e.code, round(time.monotonic() - t0, 2), err_msg
    except Exception as e:
        return False, 'canary_transport', type(e).__name__, round(time.monotonic() - t0, 2), str(e)


def api_select(data=None):
    h = {'Authorization': 'Bearer skyvpn-local-only', 'Content-Type': 'application/json'}
    url = 'http://127.0.0.1:6756/proxies' + ('/select' if data else '')
    req = urllib.request.Request(url, data=json.dumps({'name': data}).encode() if data else None, headers=h, method='PUT' if data else None)
    with urllib.request.urlopen(req, timeout=8) as r:
        b = r.read()
    return json.loads(b) if b else None


# 1. 执行轻量快速探测 (Track 1)
ok, reason, status, elapsed = probe_light()
fail_count = 0 if ok else old_state.get('failures', 0) + 1

try:
    node = api_select()['proxies']['select']['now']
except Exception:
    node = 'unavailable'

last_recovery_ts = old_state.get('last_recovery_ts', 0)
selection_path = pathlib.Path('/var/lib/skyvpn/selected-residential-node')
persisted_node = selection_path.read_text() if selection_path.exists() else None

transport_failures = ((old_state.get('transport_failures', old_state.get('failures', 0) if old_state.get('reason') == 'transport' else 0) + 1)
                      if not ok and reason == 'transport' else 0)

if not ok and reason == 'transport' and transport_failures >= 2 and (time.time() - last_recovery_ts >= 600):
    last_recovery_ts = time.time()
    try:
        subprocess.run(['/usr/local/sbin/skyvpn-select-residential', '--force'], timeout=90, capture_output=True)
        ok, reason, status, elapsed = probe_light()
        if ok:
            fail_count = 0
            transport_failures = 0
            node = api_select()['proxies']['select']['now']
    except Exception:
        ok = False
    finally:
        if not ok and node != 'unavailable':
            try:
                api_select(node)
            except Exception:
                pass
        if persisted_node is not None and not ok:
            selection_path.write_text(persisted_node)

# 2. 周期性或受控触发金丝雀带载探针 (Track 2)
force_canary = '--canary' in sys.argv
last_canary_ts = old_state.get('last_canary_ts', 0)
canary_due = force_canary or (time.time() - last_canary_ts >= CANARY_INTERVAL_SECONDS)

canary_ok = old_state.get('canary_healthy', True)
canary_reason = old_state.get('canary_reason', 'skipped')
canary_status = old_state.get('canary_status', 200)

if ok and canary_due:
    c_ok, c_reason, c_code, c_sec, c_err = probe_canary()
    last_canary_ts = time.time()
    canary_ok = c_ok
    canary_reason = c_reason
    canary_status = c_code
    if not c_ok and c_code == 429:
        # 记录关键预警日志
        log_entry = f"[{datetime.datetime.now().isoformat()}] ALERT: Canary WAF 429 Detected! Reason: {c_reason}, Body: {c_err}\n"
        try:
            with open(CANARY_LOG_PATH, 'a', encoding='utf-8') as f_log:
                f_log.write(log_entry)
        except Exception:
            pass
        subprocess.run(['logger', '-t', 'cliproxy-canary-alert', f'WAF 429 prompt signature triggered: {c_reason}'])

result = {
    'time': datetime.datetime.now().astimezone().isoformat(),
    'healthy': ok,
    'reason': reason,
    'status': status,
    'seconds': elapsed,
    'failures': fail_count,
    'node': node,
    'last_recovery_ts': last_recovery_ts,
    'transport_failures': transport_failures,
    'canary_healthy': canary_ok,
    'canary_reason': canary_reason,
    'canary_status': canary_status,
    'last_canary_ts': last_canary_ts
}

tmp_file = STATE_PATH.with_suffix('.tmp')
tmp_file.write_text(json.dumps(result, ensure_ascii=False))
os.replace(tmp_file, STATE_PATH)

subprocess.run(['logger', '-t', 'cliproxy-functional-health', json.dumps(result, ensure_ascii=False)])
print(json.dumps(result, ensure_ascii=False))
raise SystemExit(0 if ok and canary_ok else 1)
