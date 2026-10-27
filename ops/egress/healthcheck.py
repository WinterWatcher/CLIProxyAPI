#!/usr/bin/env python3
"""
healthcheck.py - Secret-free proxy egress health detection.

Uses Python standard library only.
Evaluates proxy connectivity, egress IP consistency, and service reachability.
"""

import argparse
import json
import os
import re
import socket
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Dict, List, Optional, Tuple

# Redacted Reason Codes
CODE_OK = "OK"
CODE_ERR_IP_MISMATCH = "ERR_IP_MISMATCH"
CODE_ERR_HTTP_STATUS = "ERR_HTTP_STATUS"
CODE_ERR_TIMEOUT = "ERR_TIMEOUT"
CODE_ERR_PROXY_CONNECTION = "ERR_PROXY_CONN"
CODE_ERR_DNS_RESOLUTION = "ERR_DNS_RESOLUTION"
CODE_ERR_NETWORK = "ERR_NETWORK"
CODE_ERR_THRESHOLD_EXCEEDED = "ERR_THRESHOLD_EXCEEDED"
CODE_ERR_CONFIG = "ERR_CONFIG"


def redact_text(text: Optional[str]) -> str:
    """Sanitize any URL or text to prevent secret/token/credential leakage."""
    if not text:
        return ""
    # Redact user:password in URLs
    sanitized = re.sub(r"(://[^:]+:)[^@]+(@)", r"\1***\2", str(text))
    # Redact query parameters that look like tokens or keys
    sanitized = re.sub(
        r"([?&](?:key|token|auth|secret|password|api_key)=)[^&]+",
        r"\1[REDACTED]",
        sanitized,
        flags=re.IGNORECASE,
    )
    # Redact Bearer tokens
    sanitized = re.sub(
        r"(Bearer\s+)[A-Za-z0-9_\-\.~+/]+=*",
        r"\1[REDACTED]",
        sanitized,
        flags=re.IGNORECASE,
    )
    return sanitized


class HealthCheckConfig:
    def __init__(
        self,
        proxy_url: Optional[str] = None,
        expected_egress_ip: Optional[str] = None,
        ip_check_url: Optional[str] = None,
        google_check_url: Optional[str] = None,
        oauth_check_url: Optional[str] = None,
        cloud_code_check_url: Optional[str] = None,
        timeout_seconds: float = 5.0,
        repeat_checks: int = 1,
        failure_threshold: int = 1,
    ):
        self.proxy_url = proxy_url.strip() if proxy_url and proxy_url.strip() else None
        self.expected_egress_ip = (
            expected_egress_ip.strip() if expected_egress_ip and expected_egress_ip.strip() else None
        )
        self.ip_check_url = ip_check_url.strip() if ip_check_url and ip_check_url.strip() else None
        self.google_check_url = (
            google_check_url.strip() if google_check_url and google_check_url.strip() else None
        )
        self.oauth_check_url = (
            oauth_check_url.strip() if oauth_check_url and oauth_check_url.strip() else None
        )
        self.cloud_code_check_url = (
            cloud_code_check_url.strip() if cloud_code_check_url and cloud_code_check_url.strip() else None
        )
        self.timeout_seconds = max(0.1, float(timeout_seconds))
        self.repeat_checks = max(1, int(repeat_checks))
        self.failure_threshold = max(1, int(failure_threshold))

    @classmethod
    def from_env(cls) -> "HealthCheckConfig":
        return cls(
            proxy_url=os.environ.get("PROXY_URL"),
            expected_egress_ip=os.environ.get("EXPECTED_EGRESS_IP"),
            ip_check_url=os.environ.get("IP_CHECK_URL"),
            google_check_url=os.environ.get("GOOGLE_CHECK_URL", "https://www.google.com/generate_204"),
            oauth_check_url=os.environ.get("OAUTH_CHECK_URL", "https://accounts.google.com"),
            cloud_code_check_url=os.environ.get(
                "CLOUD_CODE_CHECK_URL", "https://cloudcode.googleapis.com"
            ),
            timeout_seconds=float(os.environ.get("CHECK_TIMEOUT", os.environ.get("TIMEOUT_SECONDS", 5.0))),
            repeat_checks=int(os.environ.get("REPEAT_CHECKS", os.environ.get("REPEATED_CHECKS", 1))),
            failure_threshold=int(os.environ.get("FAILURE_THRESHOLD", 1)),
        )


def _build_opener(proxy_url: Optional[str]) -> urllib.request.OpenerDirector:
    """Build a urllib opener configured with proxy or explicit direct transport."""
    if proxy_url:
        proxy_handler = urllib.request.ProxyHandler({
            "http": proxy_url,
            "https": proxy_url,
        })
    else:
        # Pass empty dict to avoid inheriting ambient OS proxies when none is configured
        proxy_handler = urllib.request.ProxyHandler({})
    return urllib.request.build_opener(proxy_handler)


def _check_endpoint(
    opener: urllib.request.OpenerDirector,
    url: str,
    timeout: float,
) -> Tuple[bool, str, Optional[int], Optional[str]]:
    """
    Perform a GET request to a single endpoint.
    Returns: (success, reason_code, http_status, response_body)
    """
    req = urllib.request.Request(
        url=url,
        headers={
            "User-Agent": "CLIProxyAPI-Healthcheck/1.0",
            "Accept": "*/*",
        },
    )
    try:
        with opener.open(req, timeout=timeout) as response:
            status = response.getcode()
            body = response.read(1024).decode("utf-8", errors="replace")
            # 2xx and 3xx are considered successful HTTP connectivity
            if 200 <= status < 400:
                return True, CODE_OK, status, body
            return False, CODE_ERR_HTTP_STATUS, status, body
    except urllib.error.HTTPError as ex:
        status = ex.code
        # Specific redirect or auth challenge on root may still indicate reachable network,
        # but 4xx/5xx errors generally count as HTTP failure
        return False, CODE_ERR_HTTP_STATUS, status, None
    except urllib.error.URLError as ex:
        reason = ex.reason
        if isinstance(reason, socket.timeout):
            return False, CODE_ERR_TIMEOUT, None, None
        if isinstance(reason, socket.gaierror):
            return False, CODE_ERR_DNS_RESOLUTION, None, None
        if isinstance(reason, ConnectionRefusedError):
            return False, CODE_ERR_PROXY_CONNECTION, None, None
        # Handle os/socket error representations
        reason_str = str(reason).lower()
        if "timed out" in reason_str:
            return False, CODE_ERR_TIMEOUT, None, None
        if "refused" in reason_str or "proxy" in reason_str:
            return False, CODE_ERR_PROXY_CONNECTION, None, None
        if "name or service not known" in reason_str or "nodename nor servname" in reason_str:
            return False, CODE_ERR_DNS_RESOLUTION, None, None
        return False, CODE_ERR_NETWORK, None, None
    except socket.timeout:
        return False, CODE_ERR_TIMEOUT, None, None
    except Exception:
        return False, CODE_ERR_NETWORK, None, None


def _extract_ip(body: str) -> Optional[str]:
    """Parse IP address from plain text or JSON response."""
    trimmed = body.strip()
    if trimmed.startswith("{"):
        try:
            data = json.loads(trimmed)
            for key in ("ip", "origin", "query", "client_ip", "address"):
                if key in data and isinstance(data[key], str):
                    return data[key].strip()
        except Exception:
            pass
    # Regex extract first IPv4 address
    match = re.search(r"\b(?:\d{1,3}\.){3}\d{1,3}\b", trimmed)
    if match:
        return match.group(0)
    return None


def run_single_health_round(
    config: HealthCheckConfig,
    opener: urllib.request.OpenerDirector,
) -> Tuple[bool, str, Dict[str, Any]]:
    """
    Execute one round of health checks against all configured endpoints.
    Returns: (is_healthy, reason_code, details_dict)
    """
    round_details: Dict[str, Any] = {"checks": {}}
    first_failure_code = CODE_OK

    # 1. Google check URL
    if config.google_check_url:
        ok, code, status, _ = _check_endpoint(opener, config.google_check_url, config.timeout_seconds)
        round_details["checks"]["google"] = {
            "url": redact_text(config.google_check_url),
            "ok": ok,
            "status": status,
            "code": code,
        }
        if not ok and first_failure_code == CODE_OK:
            first_failure_code = code

    # 2. OAuth check URL
    if config.oauth_check_url:
        ok, code, status, _ = _check_endpoint(opener, config.oauth_check_url, config.timeout_seconds)
        round_details["checks"]["oauth"] = {
            "url": redact_text(config.oauth_check_url),
            "ok": ok,
            "status": status,
            "code": code,
        }
        if not ok and first_failure_code == CODE_OK:
            first_failure_code = code

    # 3. Cloud Code check URL
    if config.cloud_code_check_url:
        ok, code, status, _ = _check_endpoint(opener, config.cloud_code_check_url, config.timeout_seconds)
        round_details["checks"]["cloud_code"] = {
            "url": redact_text(config.cloud_code_check_url),
            "ok": ok,
            "status": status,
            "code": code,
        }
        if not ok and first_failure_code == CODE_OK:
            first_failure_code = code

    # 4. Expected Egress IP verification
    detected_ip: Optional[str] = None
    if config.expected_egress_ip:
        check_url = config.ip_check_url or "https://api.ipify.org"
        ok, code, status, body = _check_endpoint(opener, check_url, config.timeout_seconds)
        if ok and body:
            detected_ip = _extract_ip(body)
            ip_matches = (detected_ip == config.expected_egress_ip)
            if not ip_matches:
                ok = False
                code = CODE_ERR_IP_MISMATCH
        else:
            if ok:
                ok = False
                code = CODE_ERR_IP_MISMATCH

        round_details["checks"]["egress_ip"] = {
            "url": redact_text(check_url),
            "ok": ok,
            "status": status,
            "code": code,
            "expected_ip": config.expected_egress_ip,
            "detected_ip": detected_ip,
        }
        if not ok and first_failure_code == CODE_OK:
            first_failure_code = code

    is_round_healthy = (first_failure_code == CODE_OK)
    return is_round_healthy, first_failure_code, round_details


def run_health_checks(config: Optional[HealthCheckConfig] = None) -> Dict[str, Any]:
    """
    Run health detection across repeat rounds with failure threshold evaluation.
    """
    if config is None:
        config = HealthCheckConfig.from_env()

    opener = _build_opener(config.proxy_url)
    rounds: List[Dict[str, Any]] = []
    failure_counts = 0
    primary_failure_code = CODE_OK

    for round_idx in range(1, config.repeat_checks + 1):
        round_healthy, round_code, details = run_single_health_round(config, opener)
        details["round"] = round_idx
        rounds.append(details)
        if not round_healthy:
            failure_counts += 1
            if primary_failure_code == CODE_OK:
                primary_failure_code = round_code

    is_overall_healthy = (failure_counts < config.failure_threshold)
    final_code = CODE_OK
    if not is_overall_healthy:
        final_code = primary_failure_code if primary_failure_code != CODE_OK else CODE_ERR_THRESHOLD_EXCEEDED

    return {
        "healthy": is_overall_healthy,
        "reason_code": final_code,
        "failure_count": failure_counts,
        "total_rounds": config.repeat_checks,
        "failure_threshold": config.failure_threshold,
        "proxy_url": redact_text(config.proxy_url),
        "expected_egress_ip": config.expected_egress_ip,
        "rounds": rounds,
    }


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Generic secret-free proxy health detection"
    )
    parser.add_argument("--proxy-url", default=None, help="Proxy URL (HTTP/HTTPS)")
    parser.add_argument("--expected-ip", default=None, help="Expected egress IP")
    parser.add_argument("--ip-check-url", default=None, help="URL to verify egress IP")
    parser.add_argument("--google-url", default=None, help="Google connectivity check URL")
    parser.add_argument("--oauth-url", default=None, help="OAuth connectivity check URL")
    parser.add_argument("--cloud-code-url", default=None, help="Cloud Code check URL")
    parser.add_argument("--timeout", type=float, default=None, help="Timeout in seconds")
    parser.add_argument("--repeat", type=int, default=None, help="Repeat check rounds")
    parser.add_argument("--threshold", type=int, default=None, help="Failure threshold")
    parser.add_argument("--json", action="store_true", help="Output JSON result")
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    env_cfg = HealthCheckConfig.from_env()

    config = HealthCheckConfig(
        proxy_url=args.proxy_url if args.proxy_url is not None else env_cfg.proxy_url,
        expected_egress_ip=args.expected_ip if args.expected_ip is not None else env_cfg.expected_egress_ip,
        ip_check_url=args.ip_check_url if args.ip_check_url is not None else env_cfg.ip_check_url,
        google_check_url=args.google_url if args.google_url is not None else env_cfg.google_check_url,
        oauth_check_url=args.oauth_url if args.oauth_url is not None else env_cfg.oauth_check_url,
        cloud_code_check_url=args.cloud_code_url if args.cloud_code_url is not None else env_cfg.cloud_code_check_url,
        timeout_seconds=args.timeout if args.timeout is not None else env_cfg.timeout_seconds,
        repeat_checks=args.repeat if args.repeat is not None else env_cfg.repeat_checks,
        failure_threshold=args.threshold if args.threshold is not None else env_cfg.failure_threshold,
    )

    result = run_health_checks(config)

    if args.json:
        print(json.dumps(result, indent=2))
    else:
        status_str = "HEALTHY" if result["healthy"] else "UNHEALTHY"
        print(
            f"[{status_str}] reason={result['reason_code']} "
            f"failures={result['failure_count']}/{result['total_rounds']} "
            f"(threshold={result['failure_threshold']})"
        )

    return 0 if result["healthy"] else 1


if __name__ == "__main__":
    sys.exit(main())
