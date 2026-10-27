#!/usr/bin/env python3
"""
select_node.py - Intelligent, secret-free proxy node selection with health verification and rollback.

Uses Python standard library only.
Discovers nodes from a local proxy controller API, filters by country prefix,
probes latency concurrently, applies user preferences, switches active node,
verifies via multi-round post-selection health checks, and preserves/rolls back to
the initial active node on failure.
"""

import argparse
import concurrent.futures
import json
import os
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Dict, List, Optional, Tuple

# Import healthcheck module from the same directory
try:
    from ops.egress.healthcheck import (
        HealthCheckConfig,
        redact_text,
        run_health_checks,
    )
except ImportError:
    from healthcheck import (
        HealthCheckConfig,
        redact_text,
        run_health_checks,
    )

# Redacted Reason Codes for Node Selection
CODE_SELECT_OK = "SELECT_OK"
CODE_ERR_NO_CANDIDATES = "ERR_NO_CANDIDATES"
CODE_ERR_PROBE_FAILED = "ERR_PROBE_FAILED"
CODE_ERR_POST_CHECK_FAILED = "ERR_POST_CHECK_FAILED"
CODE_ROLLBACK_SUCCESS = "ROLLBACK_SUCCESS"
CODE_ROLLBACK_FAILED = "ROLLBACK_FAILED"
CODE_ERR_API_UNAVAILABLE = "ERR_API_UNAVAILABLE"


class NodeSelectionConfig:
    def __init__(
        self,
        node_api_base_url: str = "http://127.0.0.1:9090",
        node_api_token: Optional[str] = None,
        selector_name: str = "GLOBAL",
        allowed_country_prefix: Optional[str] = None,
        preferred_node_names: Optional[str] = None,
        probe_concurrency: int = 5,
        probe_timeout_ms: int = 3000,
        delay_test_url: str = "https://www.google.com/generate_204",
        post_select_health_rounds: int = 2,
        post_select_health_interval: float = 0.5,
        health_config: Optional[HealthCheckConfig] = None,
    ):
        self.node_api_base_url = node_api_base_url.rstrip("/")
        self.node_api_token = node_api_token.strip() if node_api_token and node_api_token.strip() else None
        self.selector_name = selector_name.strip() if selector_name else "GLOBAL"
        self.allowed_country_prefix = (
            allowed_country_prefix.strip() if allowed_country_prefix and allowed_country_prefix.strip() else None
        )
        self.preferred_node_names = (
            preferred_node_names.strip() if preferred_node_names and preferred_node_names.strip() else None
        )
        self.probe_concurrency = max(1, int(probe_concurrency))
        self.probe_timeout_ms = max(100, int(probe_timeout_ms))
        self.delay_test_url = delay_test_url.strip() if delay_test_url else "https://www.google.com/generate_204"
        self.post_select_health_rounds = max(1, int(post_select_health_rounds))
        self.post_select_health_interval = max(0.0, float(post_select_health_interval))
        self.health_config = health_config or HealthCheckConfig.from_env()

    @classmethod
    def from_env(cls) -> "NodeSelectionConfig":
        return cls(
            node_api_base_url=os.environ.get("NODE_API_BASE_URL", "http://127.0.0.1:9090"),
            node_api_token=os.environ.get("NODE_API_TOKEN"),
            selector_name=os.environ.get("SELECTOR_NAME", "GLOBAL"),
            allowed_country_prefix=os.environ.get("ALLOWED_COUNTRY_PREFIX"),
            preferred_node_names=os.environ.get("PREFERRED_NODE_NAMES"),
            probe_concurrency=int(os.environ.get("PROBE_CONCURRENCY", 5)),
            probe_timeout_ms=int(os.environ.get("PROBE_TIMEOUT_MS", 3000)),
            delay_test_url=os.environ.get("DELAY_TEST_URL", "https://www.google.com/generate_204"),
            post_select_health_rounds=int(os.environ.get("POST_SELECT_HEALTH_ROUNDS", 2)),
            post_select_health_interval=float(os.environ.get("POST_SELECT_HEALTH_INTERVAL", 0.5)),
            health_config=HealthCheckConfig.from_env(),
        )


class NodeApiClient:
    """HTTP client for local controller API, bypassing proxies and redacting tokens."""

    def __init__(self, base_url: str, token: Optional[str] = None, timeout: float = 5.0):
        self.base_url = base_url.rstrip("/")
        self.token = token
        self.timeout = timeout
        # Explicit empty ProxyHandler prevents local controller requests from being proxied
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def _get_headers(self) -> Dict[str, str]:
        headers = {
            "Accept": "application/json",
            "User-Agent": "CLIProxyAPI-NodeSelector/1.0",
        }
        if self.token:
            headers["Authorization"] = f"Bearer {self.token}"
        return headers

    def get_selector(self, selector_name: str) -> Tuple[Optional[str], List[str]]:
        """
        Fetch selector state.
        Returns: (current_node_name, list_of_all_candidates)
        """
        quoted = urllib.parse.quote(selector_name, safe="")
        url = f"{self.base_url}/proxies/{quoted}"
        req = urllib.request.Request(url, headers=self._get_headers(), method="GET")

        try:
            with self.opener.open(req, timeout=self.timeout) as resp:
                data = json.loads(resp.read().decode("utf-8"))
                current_node = data.get("now")
                candidates = data.get("all", [])
                return current_node, candidates
        except urllib.error.HTTPError as ex:
            if ex.code == 404:
                # Fallback: Query all proxies
                return self._get_selector_from_all_proxies(selector_name)
            raise
        except Exception:
            raise

    def _get_selector_from_all_proxies(self, selector_name: str) -> Tuple[Optional[str], List[str]]:
        url = f"{self.base_url}/proxies"
        req = urllib.request.Request(url, headers=self._get_headers(), method="GET")
        with self.opener.open(req, timeout=self.timeout) as resp:
            data = json.loads(resp.read().decode("utf-8"))
            proxies = data.get("proxies", {})
            selector_data = proxies.get(selector_name, {})
            return selector_data.get("now"), selector_data.get("all", [])

    def probe_delay(self, node_name: str, test_url: str, timeout_ms: int) -> Optional[int]:
        """
        Probe delay for a single node via controller API.
        Returns: delay in milliseconds or None if unreachable.
        """
        quoted_node = urllib.parse.quote(node_name, safe="")
        query = urllib.parse.urlencode({
            "url": test_url,
            "timeout": timeout_ms,
        })
        url = f"{self.base_url}/proxies/{quoted_node}/delay?{query}"
        req = urllib.request.Request(url, headers=self._get_headers(), method="GET")

        timeout_sec = (timeout_ms / 1000.0) + 1.0
        try:
            with self.opener.open(req, timeout=timeout_sec) as resp:
                data = json.loads(resp.read().decode("utf-8"))
                delay = data.get("delay")
                if isinstance(delay, (int, float)) and delay > 0:
                    return int(delay)
                return None
        except Exception:
            return None

    def select_node(self, selector_name: str, node_name: str) -> bool:
        """
        Select active node for selector via PUT /proxies/{selector}.
        """
        quoted = urllib.parse.quote(selector_name, safe="")
        url = f"{self.base_url}/proxies/{quoted}"
        body = json.dumps({"name": node_name}).encode("utf-8")
        headers = self._get_headers()
        headers["Content-Type"] = "application/json"

        req = urllib.request.Request(url, data=body, headers=headers, method="PUT")
        try:
            with self.opener.open(req, timeout=self.timeout) as resp:
                return 200 <= resp.getcode() < 300
        except Exception:
            return False


def matches_country_prefix(node_name: str, allowed_prefixes: List[str]) -> bool:
    """Check if node name matches any of the allowed country prefixes."""
    if not allowed_prefixes:
        return True
    upper_name = node_name.upper()
    for prefix in allowed_prefixes:
        p = prefix.strip().upper()
        if not p:
            continue
        # Direct prefix match (e.g. "US - 01", "US_Node")
        if upper_name.startswith(p):
            return True
        # Bracketed prefix match (e.g. "[US] Node", "(US) Node")
        if f"[{p}]" in upper_name or f"({p})" in upper_name:
            return True
        # Word boundary match
        if re.search(rf"\b{re.escape(p)}\b", upper_name):
            return True
    return False


def compute_preference_rank(node_name: str, preferred_list: List[str]) -> int:
    """Return 0-based priority rank for preferred nodes (lower is more preferred)."""
    if not preferred_list:
        return 0
    upper_name = node_name.upper()
    for rank, pref in enumerate(preferred_list):
        p = pref.strip().upper()
        if p and p in upper_name:
            return rank
    return len(preferred_list)


def probe_candidates_concurrently(
    client: NodeApiClient,
    candidates: List[str],
    test_url: str,
    timeout_ms: int,
    concurrency: int,
) -> Dict[str, int]:
    """
    Concurrently test delay for candidate nodes.
    Returns: map of node_name -> delay_ms for successful probes.
    """
    alive_delays: Dict[str, int] = {}
    with concurrent.futures.ThreadPoolExecutor(max_workers=concurrency) as executor:
        future_to_node = {
            executor.submit(client.probe_delay, node, test_url, timeout_ms): node
            for node in candidates
        }
        for future in concurrent.futures.as_completed(future_to_node):
            node = future_to_node[future]
            try:
                delay = future.result()
                if delay is not None:
                    alive_delays[node] = delay
            except Exception:
                pass
    return alive_delays


def select_best_node(config: Optional[NodeSelectionConfig] = None) -> Dict[str, Any]:
    """
    Execute complete node discovery, filtering, probing, selection, multi-round
    post-selection verification, and rollback if verification fails.
    """
    if config is None:
        config = NodeSelectionConfig.from_env()

    client = NodeApiClient(config.node_api_base_url, config.node_api_token)

    # 1. Fetch current selector state
    try:
        original_node, all_candidates = client.get_selector(config.selector_name)
    except Exception as ex:
        return {
            "success": False,
            "reason_code": CODE_ERR_API_UNAVAILABLE,
            "message": f"Failed to connect to local node API: {redact_text(str(ex))}",
            "current_node": None,
            "selected_node": None,
        }

    # Exclude system/virtual entries
    excluded_names = {"DIRECT", "REJECT", "GLOBAL", config.selector_name}
    candidates = [c for c in all_candidates if c not in excluded_names]

    # 2. Filter candidates by country prefix
    allowed_prefixes = (
        [p.strip() for p in config.allowed_country_prefix.split(",") if p.strip()]
        if config.allowed_country_prefix
        else []
    )
    filtered_candidates = [
        c for c in candidates if matches_country_prefix(c, allowed_prefixes)
    ]

    if not filtered_candidates:
        return {
            "success": False,
            "reason_code": CODE_ERR_NO_CANDIDATES,
            "message": "No candidates matched allowed country prefixes",
            "current_node": original_node,
            "selected_node": original_node,
        }

    # 3. Concurrent delay probing
    alive_delays = probe_candidates_concurrently(
        client=client,
        candidates=filtered_candidates,
        test_url=config.delay_test_url,
        timeout_ms=config.probe_timeout_ms,
        concurrency=config.probe_concurrency,
    )

    if not alive_delays:
        return {
            "success": False,
            "reason_code": CODE_ERR_PROBE_FAILED,
            "message": "All probed candidates failed latency check",
            "current_node": original_node,
            "selected_node": original_node,
        }

    # 4. Rank candidates by (preference_rank, delay)
    preferred_list = (
        [p.strip() for p in config.preferred_node_names.split(",") if p.strip()]
        if config.preferred_node_names
        else []
    )

    ranked_candidates = sorted(
        alive_delays.keys(),
        key=lambda n: (
            compute_preference_rank(n, preferred_list),
            alive_delays[n],
        ),
    )

    # 5. Selection and multi-round post-selection verification
    selection_succeeded = False
    chosen_node: Optional[str] = None
    failed_candidates: List[str] = []

    for candidate in ranked_candidates:
        # Switch selector to candidate
        switched = client.select_node(config.selector_name, candidate)
        if not switched:
            failed_candidates.append(candidate)
            continue

        # Multi-round post-selection health verification
        all_rounds_passed = True
        for round_idx in range(1, config.post_select_health_rounds + 1):
            if round_idx > 1 and config.post_select_health_interval > 0:
                time.sleep(config.post_select_health_interval)

            health_res = run_health_checks(config.health_config)
            if not health_res["healthy"]:
                all_rounds_passed = False
                break

        if all_rounds_passed:
            selection_succeeded = True
            chosen_node = candidate
            break
        else:
            failed_candidates.append(candidate)

    # 6. Success or Rollback to preserve current node
    if selection_succeeded and chosen_node:
        return {
            "success": True,
            "reason_code": CODE_SELECT_OK,
            "current_node": chosen_node,
            "selected_node": chosen_node,
            "previous_node": original_node,
            "delay_ms": alive_delays.get(chosen_node),
            "candidates_count": len(ranked_candidates),
        }

    # Selection failed: preserve / rollback to original node
    rollback_ok = False
    if original_node:
        rollback_ok = client.select_node(config.selector_name, original_node)

    rollback_code = CODE_ROLLBACK_SUCCESS if rollback_ok else CODE_ROLLBACK_FAILED
    return {
        "success": False,
        "reason_code": rollback_code,
        "primary_error": CODE_ERR_POST_CHECK_FAILED,
        "message": f"Selection failed for candidates {failed_candidates}; preserved original node",
        "current_node": original_node if rollback_ok else None,
        "selected_node": original_node,
        "previous_node": original_node,
        "failed_candidates": failed_candidates,
    }


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Select optimal proxy node with health verification and automatic rollback"
    )
    parser.add_argument("--api-base-url", default=None, help="Local node API base URL")
    parser.add_argument("--token", default=None, help="API authentication token")
    parser.add_argument("--selector", default=None, help="Selector group name")
    parser.add_argument("--allowed-prefix", default=None, help="Comma-separated country prefixes")
    parser.add_argument("--preferred", default=None, help="Comma-separated preferred node names")
    parser.add_argument("--concurrency", type=int, default=None, help="Probe concurrency")
    parser.add_argument("--timeout-ms", type=int, default=None, help="Probe timeout ms")
    parser.add_argument("--health-rounds", type=int, default=None, help="Post-selection health check rounds")
    parser.add_argument("--json", action="store_true", help="Output JSON result")
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    env_cfg = NodeSelectionConfig.from_env()

    config = NodeSelectionConfig(
        node_api_base_url=args.api_base_url if args.api_base_url is not None else env_cfg.node_api_base_url,
        node_api_token=args.token if args.token is not None else env_cfg.node_api_token,
        selector_name=args.selector if args.selector is not None else env_cfg.selector_name,
        allowed_country_prefix=args.allowed_prefix if args.allowed_prefix is not None else env_cfg.allowed_country_prefix,
        preferred_node_names=args.preferred if args.preferred is not None else env_cfg.preferred_node_names,
        probe_concurrency=args.concurrency if args.concurrency is not None else env_cfg.probe_concurrency,
        probe_timeout_ms=args.timeout_ms if args.timeout_ms is not None else env_cfg.probe_timeout_ms,
        post_select_health_rounds=args.health_rounds if args.health_rounds is not None else env_cfg.post_select_health_rounds,
        health_config=HealthCheckConfig.from_env(),
    )

    result = select_best_node(config)

    if args.json:
        print(json.dumps(result, indent=2))
    else:
        status = "SUCCESS" if result["success"] else "FAILED"
        print(
            f"[{status}] reason={result['reason_code']} "
            f"selected={result.get('selected_node')} "
            f"current={result.get('current_node')}"
        )

    return 0 if result["success"] else 1


if __name__ == "__main__":
    sys.exit(main())
