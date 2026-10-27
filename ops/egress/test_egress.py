#!/usr/bin/env python3
"""
test_egress.py - Unit tests for egress proxy healthcheck and node selection.

Tests use Python standard library only and run against local mock HTTP servers:
- Pass: healthy check across all endpoints.
- IP mismatch: detected egress IP != expected IP.
- HTTP failure: endpoint returns 500 status.
- Node selection success: discovers nodes, filters, probes, selects best node, post-check passes.
- Failed selection rollback: post-selection check fails, rolls back to original active node.
"""

import json
import threading
import unittest
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from ops.egress.healthcheck import (
    CODE_ERR_HTTP_STATUS,
    CODE_ERR_IP_MISMATCH,
    CODE_OK,
    HealthCheckConfig,
    redact_text,
    run_health_checks,
)
from ops.egress.select_node import (
    CODE_ROLLBACK_SUCCESS,
    CODE_SELECT_OK,
    NodeApiClient,
    NodeSelectionConfig,
    select_best_node,
)


class MockHealthRequestHandler(BaseHTTPRequestHandler):
    """Mock HTTP handler simulating Google, OAuth, Cloud Code, and IP check endpoints."""

    # Configurable responses on class or instance
    response_code = 200
    egress_ip = "198.51.100.1"
    ip_status = 200
    google_status = 204
    oauth_status = 200
    cloud_code_status = 200

    def log_message(self, format, *args):
        # Suppress server stderr logging during test runs
        pass

    def do_GET(self):
        parsed = urllib.parse.urlparse(self.path)
        path = parsed.path

        if path == "/google":
            self.send_response(self.google_status)
            self.end_headers()
            if self.google_status == 200:
                self.wfile.write(b"OK")
        elif path == "/oauth":
            self.send_response(self.oauth_status)
            self.end_headers()
            self.wfile.write(b'{"status": "authenticated"}')
        elif path == "/cloudcode":
            self.send_response(self.cloud_code_status)
            self.end_headers()
            self.wfile.write(b'{"ready": true}')
        elif path == "/ip":
            self.send_response(self.ip_status)
            self.send_header("Content-Type", "text/plain")
            self.end_headers()
            self.wfile.write(self.egress_ip.encode("utf-8"))
        elif path == "/ip-json":
            self.send_response(self.ip_status)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            body = json.dumps({"origin": self.egress_ip})
            self.wfile.write(body.encode("utf-8"))
        else:
            self.send_response(404)
            self.end_headers()


class MockNodeApiHandler(BaseHTTPRequestHandler):
    """Mock HTTP handler simulating local proxy controller REST API."""

    current_node = "Old-Node"
    candidates = ["Old-Node", "US-Node-1", "US-Node-2", "HK-Node-1"]
    delays = {
        "US-Node-1": 60,
        "US-Node-2": 150,
        "HK-Node-1": 40,
        "Old-Node": 200,
    }
    recorded_switches = []

    def log_message(self, format, *args):
        pass

    def do_GET(self):
        parsed = urllib.parse.urlparse(self.path)
        path = parsed.path

        # GET /proxies/GLOBAL
        if path == "/proxies/GLOBAL":
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            payload = {
                "name": "GLOBAL",
                "type": "Selector",
                "now": self.current_node,
                "all": self.candidates,
            }
            self.wfile.write(json.dumps(payload).encode("utf-8"))
            return

        # GET /proxies/{name}/delay?url=...&timeout=...
        if "/delay" in path:
            node_name = path.replace("/proxies/", "").replace("/delay", "")
            node_name = urllib.parse.unquote(node_name)
            delay = self.delays.get(node_name)
            if delay is not None and delay > 0:
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps({"delay": delay}).encode("utf-8"))
            else:
                self.send_response(504)
                self.end_headers()
            return

        # GET /proxies
        if path == "/proxies":
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            payload = {
                "proxies": {
                    "GLOBAL": {
                        "name": "GLOBAL",
                        "now": self.current_node,
                        "all": self.candidates,
                    }
                }
            }
            self.wfile.write(json.dumps(payload).encode("utf-8"))
            return

        self.send_response(404)
        self.end_headers()

    def do_PUT(self):
        parsed = urllib.parse.urlparse(self.path)
        if parsed.path == "/proxies/GLOBAL":
            content_length = int(self.headers.get("Content-Length", 0))
            body = self.rfile.read(content_length).decode("utf-8")
            data = json.loads(body)
            target_name = data.get("name")
            self.__class__.current_node = target_name
            self.__class__.recorded_switches.append(target_name)

            self.send_response(204)
            self.end_headers()
            return

        self.send_response(404)
        self.end_headers()


class TestEgressHealthAndSelection(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        # Start mock health server
        cls.health_server = ThreadingHTTPServer(("127.0.0.1", 0), MockHealthRequestHandler)
        cls.health_port = cls.health_server.server_port
        cls.health_thread = threading.Thread(target=cls.health_server.serve_forever, daemon=True)
        cls.health_thread.start()

        # Start mock node API server
        cls.node_server = ThreadingHTTPServer(("127.0.0.1", 0), MockNodeApiHandler)
        cls.node_port = cls.node_server.server_port
        cls.node_thread = threading.Thread(target=cls.node_server.serve_forever, daemon=True)
        cls.node_thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.health_server.shutdown()
        cls.health_server.server_close()
        cls.node_server.shutdown()
        cls.node_server.server_close()

    def setUp(self):
        # Reset server handlers to clean defaults
        MockHealthRequestHandler.google_status = 204
        MockHealthRequestHandler.oauth_status = 200
        MockHealthRequestHandler.cloud_code_status = 200
        MockHealthRequestHandler.ip_status = 200
        MockHealthRequestHandler.egress_ip = "198.51.100.1"

        MockNodeApiHandler.current_node = "Old-Node"
        MockNodeApiHandler.candidates = ["Old-Node", "US-Node-1", "US-Node-2", "HK-Node-1"]
        MockNodeApiHandler.delays = {
            "US-Node-1": 60,
            "US-Node-2": 150,
            "HK-Node-1": 40,
            "Old-Node": 200,
        }
        MockNodeApiHandler.recorded_switches = []

    def test_healthcheck_pass(self):
        """Case 1: All endpoints healthy, egress IP matches expected IP."""
        cfg = HealthCheckConfig(
            proxy_url=None,
            expected_egress_ip="198.51.100.1",
            ip_check_url=f"http://127.0.0.1:{self.health_port}/ip",
            google_check_url=f"http://127.0.0.1:{self.health_port}/google",
            oauth_check_url=f"http://127.0.0.1:{self.health_port}/oauth",
            cloud_code_check_url=f"http://127.0.0.1:{self.health_port}/cloudcode",
            timeout_seconds=2.0,
            repeat_checks=2,
            failure_threshold=1,
        )

        result = run_health_checks(cfg)
        self.assertTrue(result["healthy"])
        self.assertEqual(result["reason_code"], CODE_OK)
        self.assertEqual(result["failure_count"], 0)

    def test_healthcheck_ip_mismatch(self):
        """Case 2: Egress IP differs from EXPECTED_EGRESS_IP."""
        MockHealthRequestHandler.egress_ip = "203.0.113.88"  # Different IP

        cfg = HealthCheckConfig(
            proxy_url=None,
            expected_egress_ip="198.51.100.1",
            ip_check_url=f"http://127.0.0.1:{self.health_port}/ip",
            google_check_url=f"http://127.0.0.1:{self.health_port}/google",
            oauth_check_url=f"http://127.0.0.1:{self.health_port}/oauth",
            cloud_code_check_url=f"http://127.0.0.1:{self.health_port}/cloudcode",
            timeout_seconds=2.0,
            repeat_checks=1,
            failure_threshold=1,
        )

        result = run_health_checks(cfg)
        self.assertFalse(result["healthy"])
        self.assertEqual(result["reason_code"], CODE_ERR_IP_MISMATCH)
        self.assertEqual(result["failure_count"], 1)

    def test_healthcheck_http_failure(self):
        """Case 3: Endpoint returns HTTP 500 error, exceeding failure threshold."""
        MockHealthRequestHandler.google_status = 500

        cfg = HealthCheckConfig(
            proxy_url=None,
            google_check_url=f"http://127.0.0.1:{self.health_port}/google",
            oauth_check_url=f"http://127.0.0.1:{self.health_port}/oauth",
            cloud_code_check_url=f"http://127.0.0.1:{self.health_port}/cloudcode",
            timeout_seconds=2.0,
            repeat_checks=2,
            failure_threshold=1,
        )

        result = run_health_checks(cfg)
        self.assertFalse(result["healthy"])
        self.assertEqual(result["reason_code"], CODE_ERR_HTTP_STATUS)
        self.assertEqual(result["failure_count"], 2)

    def test_node_selection_success(self):
        """Case 4: Node selection filters, probes, picks preferred low-latency node, and post-check passes."""
        health_cfg = HealthCheckConfig(
            proxy_url=None,
            google_check_url=f"http://127.0.0.1:{self.health_port}/google",
            oauth_check_url=f"http://127.0.0.1:{self.health_port}/oauth",
            cloud_code_check_url=f"http://127.0.0.1:{self.health_port}/cloudcode",
            timeout_seconds=2.0,
            repeat_checks=1,
        )

        node_cfg = NodeSelectionConfig(
            node_api_base_url=f"http://127.0.0.1:{self.node_port}",
            node_api_token="test_secret_token",
            selector_name="GLOBAL",
            allowed_country_prefix="US",  # HK node should be filtered out
            preferred_node_names="Node-1",  # US-Node-1 preferred over US-Node-2
            probe_concurrency=4,
            probe_timeout_ms=1000,
            delay_test_url=f"http://127.0.0.1:{self.health_port}/google",
            post_select_health_rounds=2,
            post_select_health_interval=0.05,
            health_config=health_cfg,
        )

        result = select_best_node(node_cfg)
        self.assertTrue(result["success"])
        self.assertEqual(result["reason_code"], CODE_SELECT_OK)
        self.assertEqual(result["selected_node"], "US-Node-1")
        self.assertEqual(result["current_node"], "US-Node-1")
        self.assertEqual(result["previous_node"], "Old-Node")
        self.assertEqual(MockNodeApiHandler.current_node, "US-Node-1")

    def test_failed_selection_rollback(self):
        """Case 5: Candidate fails post-selection healthcheck, rolls back to preserve original node."""
        # Candidate delay works, but healthcheck fails with 500
        MockHealthRequestHandler.google_status = 500

        health_cfg = HealthCheckConfig(
            proxy_url=None,
            google_check_url=f"http://127.0.0.1:{self.health_port}/google",
            oauth_check_url=f"http://127.0.0.1:{self.health_port}/oauth",
            cloud_code_check_url=f"http://127.0.0.1:{self.health_port}/cloudcode",
            timeout_seconds=1.0,
            repeat_checks=1,
            failure_threshold=1,
        )

        node_cfg = NodeSelectionConfig(
            node_api_base_url=f"http://127.0.0.1:{self.node_port}",
            node_api_token="test_secret_token",
            selector_name="GLOBAL",
            allowed_country_prefix="US",
            probe_concurrency=2,
            probe_timeout_ms=1000,
            delay_test_url=f"http://127.0.0.1:{self.health_port}/oauth",
            post_select_health_rounds=1,
            post_select_health_interval=0.01,
            health_config=health_cfg,
        )

        result = select_best_node(node_cfg)
        self.assertFalse(result["success"])
        self.assertEqual(result["reason_code"], CODE_ROLLBACK_SUCCESS)
        self.assertEqual(result["current_node"], "Old-Node")
        self.assertEqual(result["selected_node"], "Old-Node")
        # Ensure the final active node in controller API is Old-Node (restored)
        self.assertEqual(MockNodeApiHandler.current_node, "Old-Node")

    def test_redact_secrets(self):
        """Verify tokens, passwords, and secrets are sanitized."""
        raw_url = "http://admin:my_secret_pass@127.0.0.1:9090/check?token=SUPER_SECRET_123"
        sanitized = redact_text(raw_url)
        self.assertNotIn("my_secret_pass", sanitized)
        self.assertNotIn("SUPER_SECRET_123", sanitized)
        self.assertIn("***@", sanitized)
        self.assertIn("[REDACTED]", sanitized)

        bearer_header = "Authorization: Bearer my_secret_bearer_token"
        sanitized_header = redact_text(bearer_header)
        self.assertNotIn("my_secret_bearer_token", sanitized_header)
        self.assertIn("[REDACTED]", sanitized_header)


if __name__ == "__main__":
    unittest.main()
