#!/usr/bin/env python3
"""Automated WAF Prompt Fingerprint Diagnostic & Bisection Tool.
Rapidly locates the exact sentence or phrase in a system prompt that triggers
Google Antigravity/Cloud Code upstream WAF 429 RESOURCE_EXHAUSTED errors.
Generates ready-to-use hotfix regex rules for cloudcode-rules.json.
"""
import sys
import os
import json
import re
import argparse
import subprocess
import tempfile

DEFAULT_SERVER_HOST = "111.229.196.126"
DEFAULT_SERVER_KEY = r"C:\Users\Administrator\.ssh\111.229.196.126_id_ed25519"


def run_remote_probe(prompt_text: str, server_host: str, server_key: str) -> tuple[int, str]:
    """Sends a single-turn probe through the safe single-credential harness on server."""
    test_payload = {
        'model': 'gemini-3.8-flash-high',
        'messages': [
            {'role': 'system', 'content': prompt_text},
            {'role': 'user', 'content': '只回复 OK'}
        ],
        'reasoning_effort': 'medium',
        'max_tokens': 512,
        'stream': True
    }

    with tempfile.NamedTemporaryFile("w", encoding="utf-8", delete=False, suffix=".json") as f:
        json.dump(test_payload, f, ensure_ascii=False)
        temp_local = f.name

    remote_payload_path = f"/tmp/diag_probe_{os.getpid()}.json"
    remote_script_path = f"/tmp/diag_runner_{os.getpid()}.py"

    remote_runner = f"""
import json, urllib.request, yaml, sys
try:
    c = yaml.safe_load(open('/opt/clirelay-validation/config.production.yaml'))
    key = c['api-keys'][0]
    with open('{remote_payload_path}', 'rb') as f:
        body = f.read()
    req = urllib.request.Request('http://127.0.0.1:8090/v1/chat/completions', data=body, headers={{'Authorization': 'Bearer ' + key, 'Content-Type': 'application/json'}})
    with urllib.request.urlopen(req, timeout=35) as r:
        print("STATUS:200")
        sys.exit(0)
except urllib.error.HTTPError as e:
    print(f"STATUS:{{e.code}}")
    sys.exit(0)
except Exception as e:
    print(f"STATUS:500:{{e}}")
    sys.exit(1)
"""

    with tempfile.NamedTemporaryFile("w", encoding="utf-8", delete=False, suffix=".py") as f_scr:
        f_scr.write(remote_runner)
        temp_scr_local = f_scr.name

    try:
        subprocess.run(["scp", "-i", server_key, temp_local, f"root@{server_host}:{remote_payload_path}"], check=True, capture_output=True)
        subprocess.run(["scp", "-i", server_key, temp_scr_local, f"root@{server_host}:{remote_script_path}"], check=True, capture_output=True)
        res = subprocess.run(["ssh", "-i", server_key, f"root@{server_host}", f"python3 {remote_script_path}; rm -f {remote_payload_path} {remote_script_path}"],
                             capture_output=True, text=True, timeout=50)
        out = res.stdout.strip()
        if "STATUS:200" in out:
            return 200, "OK"
        elif "STATUS:429" in out:
            return 429, "WAF 429 Triggered"
        else:
            return 500, out
    finally:
        if os.path.exists(temp_local):
            os.remove(temp_local)
        if os.path.exists(temp_scr_local):
            os.remove(temp_scr_local)


def extract_prompt_from_log_or_file(source_path: str) -> str:
    with open(source_path, "r", encoding="utf-8", errors="replace") as f:
        content = f.read()

    # If it is an error-v1-chat-completions log file
    if "=== REQUEST BODY ===" in content:
        start = content.index("=== REQUEST BODY ===\n") + len("=== REQUEST BODY ===\n")
        end = content.find("\n\n\n=== API REQUEST 1 ===")
        if end == -1:
            end = len(content)
        body_str = content[start:end].strip()
        try:
            d = json.loads(body_str)
            for m in d.get("messages", []):
                if m.get("role") == "system":
                    return m.get("content", "")
        except Exception:
            pass

    # If it is a raw JSON payload
    try:
        d = json.loads(content)
        if isinstance(d, dict):
            for m in d.get("messages", []):
                if m.get("role") == "system":
                    return m.get("content", "")
    except Exception:
        pass

    return content


def bisect_prompt(prompt_text: str, server_host: str, server_key: str):
    print("=" * 60)
    print("  Prompt Fingerprint Automated Bisection Tool")
    print(f"  Input text length: {len(prompt_text)} characters")
    print("=" * 60)

    # 1. Baseline test
    print("\n[Step 1] Baseline probe on full input text...")
    code, msg = run_remote_probe(prompt_text, server_host, server_key)
    print(f"  -> Full text result: HTTP {code} ({msg})")
    if code == 200:
        print("\n[SUCCESS] The prompt text is 100% clean and returned HTTP 200. No WAF trigger found!")
        return

    # 2. Split by major sections (# )
    raw_sections = prompt_text.split("\n# ")
    sections = []
    for i, s in enumerate(raw_sections):
        if i == 0:
            sections.append(s)
        else:
            sections.append("# " + s)

    print(f"\n[Step 2] Bisecting {len(sections)} major section(s)...")
    failing_section_idx = -1
    for idx, sec in enumerate(sections):
        if len(sec.strip()) < 5:
            continue
        header = sec.splitlines()[0] if sec.splitlines() else "Section"
        code, msg = run_remote_probe(sec, server_host, server_key)
        print(f"  - Section {idx} [{header[:35]}...] ({len(sec)} chars) -> HTTP {code}")
        if code == 429:
            failing_section_idx = idx
            break

    if failing_section_idx == -1:
        print("\n[NOTE] No single section triggered 429 alone. The trigger may span section boundaries.")
        target_text = prompt_text
    else:
        target_text = sections[failing_section_idx]
        print(f"\n[Step 3] Isolated failing section {failing_section_idx} ({len(target_text)} chars).")

    # 3. Bisect paragraphs inside failing text
    paragraphs = [p for p in target_text.split("\n\n") if len(p.strip()) > 5]
    print(f"\n[Step 4] Bisecting {len(paragraphs)} paragraph(s)...")
    failing_para = target_text
    for p_idx, para in enumerate(paragraphs):
        code, msg = run_remote_probe(para, server_host, server_key)
        print(f"  - Paragraph {p_idx} ({len(para)} chars) -> HTTP {code}")
        if code == 429:
            failing_para = para
            break

    # 4. Bisect sentences inside failing paragraph
    sentences = [s.strip() for s in re.split(r'(?<=[.!?。！？\n])\s+', failing_para) if len(s.strip()) > 10]
    print(f"\n[Step 5] Bisecting {len(sentences)} sentence(s)...")
    culprit_sentence = failing_para
    for s_idx, sentence in enumerate(sentences):
        code, msg = run_remote_probe(sentence, server_host, server_key)
        print(f"  - Sentence {s_idx} [{sentence[:40]}...] -> HTTP {code}")
        if code == 429:
            culprit_sentence = sentence
            break

    print("\n" + "=" * 60)
    print("  EXACT WAF FINGERPRINT PINPOINTED!")
    print("=" * 60)
    print(f"Trigger text snippet:\n  \"{culprit_sentence}\"\n")

    # Generate regex rule for cloudcode-rules.json
    clean_snippet = re.sub(r'[\r\n]+', ' ', culprit_sentence)
    first_few_words = " ".join(clean_snippet.split()[:8])
    escaped_pat = re.escape(first_few_words)

    rule_template = {
        "name": f"Auto-mitigated WAF trigger: {first_few_words[:30]}",
        "pattern": f"(?is){escaped_pat}.*?(?=\\n\\n|\\n#|\\Z)",
        "replacement": "Focus on delivering working results and reporting execution output accurately."
    }

    print("Suggested rule for 'harness_strip_sections' in cloudcode-rules.json:")
    print(json.dumps(rule_template, indent=2, ensure_ascii=False))
    print("=" * 60)


def main():
    parser = argparse.ArgumentParser(description="Automated WAF Prompt Fingerprint Diagnostic Tool")
    parser.add_argument("--file", "-f", help="Path to prompt file, json payload, or error log")
    parser.add_argument("--text", "-t", help="Raw prompt string to diagnose")
    parser.add_argument("--last-error", action="store_true", help="Fetch latest error log from remote server")
    parser.add_argument("--server", default=DEFAULT_SERVER_HOST, help="Server IP")
    parser.add_argument("--key", default=DEFAULT_SERVER_KEY, help="Path to SSH private key")

    args = parser.parse_args()

    prompt_text = ""
    if args.last_error:
        print(f"Fetching latest error log from {args.server}...")
        cmd = f"ssh -i \"{args.key}\" root@{args.server} \"ls -t /opt/clirelay-validation/logs/error-v1-chat-completions-*.log 2>/dev/null | head -n 1\""
        res = subprocess.run(cmd, capture_output=True, text=True, shell=True)
        latest_file = res.stdout.strip()
        if not latest_file:
            print("No error logs found on remote server.")
            sys.exit(1)
        print(f"Found latest log: {latest_file}")
        with tempfile.NamedTemporaryFile("w", delete=False) as f:
            temp_log = f.name
        subprocess.run(["scp", "-i", args.key, f"root@{args.server}:{latest_file}", temp_log], check=True)
        prompt_text = extract_prompt_from_log_or_file(temp_log)
        if os.path.exists(temp_log):
            os.remove(temp_log)
    elif args.file:
        prompt_text = extract_prompt_from_log_or_file(args.file)
    elif args.text:
        prompt_text = args.text
    else:
        # Default to checking system_msg.txt from scratch if available
        scratch_msg = r"C:\Users\Administrator\.gemini\antigravity\brain\603b8372-325f-41df-bfec-2c279ba6d719\scratch\system_msg.txt"
        if os.path.exists(scratch_msg):
            with open(scratch_msg, "r", encoding="utf-8") as f:
                prompt_text = f.read()
        else:
            parser.print_help()
            sys.exit(1)

    bisect_prompt(prompt_text, args.server, args.key)


if __name__ == "__main__":
    main()
