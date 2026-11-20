#!/usr/bin/env python3
"""Local Test Suite for Multi-Agent Signature Stripping & Antigravity Zero-Leak Normalization.
"""
import sys
import os
import json

# 导入修改后的 relay 模块进行本地测试
sys.path.insert(0, os.path.dirname(__file__))

# 设置本地规则路径指向刚刚更新的 scripts/cloudcode-rules.json

import re

from importlib.machinery import SourceFileLoader
relay = SourceFileLoader("relay", os.path.join(os.path.dirname(__file__), "cloudcode-google-relay.py")).load_module()

# 强制重定向规则路径到本地 scripts/cloudcode-rules.json
relay.RULES_CONFIG_FILE = os.path.join(os.path.dirname(__file__), "cloudcode-rules.json")
relay.RULE_ENGINE = relay.DynamicRuleEngine(config_path=relay.RULES_CONFIG_FILE)

print("=== Starting Multi-Agent Signature Test Suite ===")

test_cases = [
    {
        "agent": "Hermes (Nous Research)",
        "payload": {
            "model": "gemini-3.8-flash-high",
            "request": {
                "contents": [
                    {
                        "role": "user",
                        "parts": [
                            {
                                "text": (
                                    "You run on Hermes Agent (by Nous Research). "
                                    "# Hermes runtime environment\n"
                                    "Active Hermes profile: developer\n"
                                    "<!-- End Hermes runtime environment -->\n"
                                    "# Google model operational directives\n"
                                    "# Tool-use enforcement\n"
                                    "# Finishing the job\n"
                                    "When the user asks you to build, run, or verify something, the deliverable is a working artifact backed by real tool output.\n"
                                    "NEVER substitute plausible-looking fabricated output.\n"
                                    "If a tool, install, or network call fails and blocks the real path, say so directly and try an alternative.\n"
                                    "Please check file /root/.hermes/config.json."
                                )
                            }
                        ]
                    }
                ]
            }
        }
    },
    {
        "agent": "OpenCode & OpenClaw",
        "payload": {
            "model": "gemini-3.8-flash-high",
            "request": {
                "contents": [
                    {
                        "role": "user",
                        "parts": [
                            {
                                "text": (
                                    "You are OpenCode, an AI coding assistant.\n"
                                    "<system-conventions>Always write clean code</system-conventions>\n"
                                    "<system-directive>Be fast</system-directive>\n"
                                    "<openclaw>Execute task</openclaw>\n"
                                    "Please analyze openclaw logic."
                                )
                            }
                        ]
                    }
                ]
            }
        }
    },
    {
        "agent": "Zed Editor",
        "payload": {
            "model": "gemini-3.8-flash-high",
            "request": {
                "contents": [
                    {
                        "role": "user",
                        "parts": [
                            {
                                "text": (
                                    "You are Zed, an AI code assistant created by Zed Industries.\n"
                                    "You are an assistant in the Zed code editor.\n"
                                    "Config file is located at ~/.config/zed/settings.json or ~/.zed."
                                )
                            }
                        ]
                    }
                ]
            }
        }
    },
    {
        "agent": "OpenAI Codex & GitHub Copilot",
        "payload": {
            "model": "gemini-3.8-flash-high",
            "request": {
                "contents": [
                    {
                        "role": "user",
                        "parts": [
                            {
                                "text": (
                                    "You are OpenAI Codex, a code agent.\n"
                                    "You are GitHub Copilot, an AI pair programmer.\n"
                                    "Follow the guidelines in copilot-instructions.md."
                                )
                            }
                        ]
                    }
                ]
            }
        }
    },
    {
        "agent": "Cursor",
        "payload": {
            "model": "gemini-3.8-flash-high",
            "stream_options": {"include_usage": True},
            "frequency_penalty": 0.5,
            "request": {
                "contents": [
                    {
                        "role": "user",
                        "parts": [
                            {
                                "text": (
                                    "You are Cursor, an AI-powered code editor.\n"
                                    "<system-reminder>Remember to use git</system-reminder>\n"
                                    "Check ~/.cursor/extensions and .cursorrules."
                                )
                            }
                        ]
                    }
                ]
            }
        }
    },
    {
        "agent": "Windsurf (Codeium)",
        "payload": {
            "model": "gemini-3.8-flash-high",
            "request": {
                "contents": [
                    {
                        "role": "user",
                        "parts": [
                            {
                                "text": (
                                    "You are Cascade, a powerful AI assistant in Windsurf developed by Codeium.\n"
                                    "Refer to ~/.windsurf and .windsurfrules."
                                )
                            }
                        ]
                    }
                ]
            }
        }
    },
    {
        "agent": "Claude Code (Anthropic)",
        "payload": {
            "model": "gemini-3.8-flash-high",
            "request": {
                "contents": [
                    {
                        "role": "user",
                        "parts": [
                            {
                                "text": (
                                    "You are Claude Code, Anthropic's official CLI for software engineering.\n"
                                    "Review CLAUDE.md and ~/.claude.json."
                                )
                            }
                        ]
                    }
                ]
            }
        }
    },
    {
        "agent": "Cline & Roo Code",
        "payload": {
            "model": "gemini-3.8-flash-high",
            "request": {
                "contents": [
                    {
                        "role": "user",
                        "parts": [
                            {
                                "text": (
                                    "You are Cline, a highly skilled software engineer.\n"
                                    "You are Roo, an experienced software engineer.\n"
                                    "<cline_task>Fix bug</cline_task>\n"
                                    "<roomarks>Bookmark 1</roomarks>\n"
                                    "Check .clinerules and .roomodes."
                                )
                            }
                        ]
                    }
                ]
            }
        }
    },
    {
        "agent": "Continue.dev & Devin & Trae",
        "payload": {
            "model": "gemini-3.8-flash-high",
            "request": {
                "contents": [
                    {
                        "role": "user",
                        "parts": [
                            {
                                "text": (
                                    "You are Continue, an open-source AI code assistant.\n"
                                    "You are Devin, an autonomous AI software engineer.\n"
                                    "You are Trae, an adaptive AI IDE.\n"
                                    "Check ~/.continue and ~/.trae."
                                )
                            }
                        ]
                    }
                ]
            }
        }
    },
    {
        "agent": "Domestic Coding Agents (MarsCode, Lingma, Comate, CodeGeeX, iFlyCode)",
        "payload": {
            "model": "gemini-3.8-flash-high",
            "request": {
                "contents": [
                    {
                        "role": "user",
                        "parts": [
                            {
                                "text": (
                                    "You are MarsCode, an AI coding assistant by ByteDance.\n"
                                    "You are Tongyi Lingma, an intelligent assistant by Alibaba Cloud.\n"
                                    "You are Baidu Comate, an AI pair programmer.\n"
                                    "You are CodeGeeX, developed by Zhipu AI.\n"
                                    "You are iFlyCode, developed by iFlytek.\n"
                                    "Config paths: ~/.marscode, ~/.lingma, ~/.codegeex."
                                )
                            }
                        ]
                    }
                ]
            }
        }
    },
    {
        "agent": "Global Coding Agents (Cody, Amazon Q, Tabnine, Replit, OpenDevin)",
        "payload": {
            "model": "gemini-3.8-flash-high",
            "request": {
                "contents": [
                    {
                        "role": "user",
                        "parts": [
                            {
                                "text": (
                                    "You are Sourcegraph Cody, an AI coding assistant.\n"
                                    "You are Amazon Q Developer, an AI code assistant.\n"
                                    "You are Tabnine.\n"
                                    "You are Replit Agent.\n"
                                    "You are OpenDevin."
                                )
                            }
                        ]
                    }
                ]
            }
        }
    },
    {
        "agent": "Thought Signature Protection Verification",
        "payload": {
            "model": "gemini-3.8-flash-high",
            "request": {
                "contents": [
                    {
                        "role": "model",
                        "parts": [
                            {
                                "thought": True,
                                "text": "Inside thought: checking if hermes or zed or cursor is mentioned",
                                "thoughtSignature": "encrypted_thought_signature_xyz_123_hermes"
                            },
                            {
                                "text": "Final answer without leak"
                            }
                        ]
                    }
                ]
            }
        }
    }
]

failed = False
for tc in test_cases:
    agent_name = tc["agent"]
    raw_bytes = json.dumps(tc["payload"]).encode("utf-8")
    sanitized_bytes, leak_hits = relay.sanitize_payload(raw_bytes)
    sanitized_obj = json.loads(sanitized_bytes.decode("utf-8"))
    
    # 验证 leak_hits
    if leak_hits > 0:
        hits, _ = relay.audit_payload_leaks(sanitized_bytes)
        print(f"[FAIL] {agent_name}: leak_hits={leak_hits}, hits={hits}")
        failed = True
    else:
        print(f"[PASS] {agent_name}: 0 leak hits")

    # 验证异构参数是否被移除 (如 stream_options, frequency_penalty)
    if "stream_options" in sanitized_obj or "frequency_penalty" in sanitized_obj:
        print(f"[FAIL] {agent_name}: Foreign parameters were not removed!")
        failed = True

    # 验证 thoughtSignature 是否被完整保留
    if agent_name == "Thought Signature Protection Verification":
        part0 = sanitized_obj["request"]["contents"][0]["parts"][0]
        if part0.get("thoughtSignature") != "encrypted_thought_signature_xyz_123_hermes":
            print(f"[FAIL] {agent_name}: thoughtSignature was corrupted: {part0.get('thoughtSignature')}")
            failed = True
        else:
            print(f"[PASS] {agent_name}: thoughtSignature intact and preserved")

if failed:
    print("\nFAILED: Some tests did not pass!")
    sys.exit(1)
else:
    print("\nSUCCESS: All multi-agent signature stripping & zero-leak tests passed cleanly!")
    sys.exit(0)
