# Egress Proxy Healthcheck & Dynamic Node Selection

Generic, secret-free proxy egress health detection and automated node selection scripts designed for production proxy environments.
Implemented exclusively using the Python standard library with zero external dependencies.

---

## Overview

1. **`healthcheck.py`**
   - Probes proxy egress connectivity across critical service endpoints.
   - Verifies public egress IP consistency against an expected address.
   - Evaluates multi-round reliability with configurable failure thresholds.
   - Emits standardized, redacted reason codes to prevent secret or credential leakage in logging pipelines.

2. **`select_node.py`**
   - Connects to a local proxy controller REST API (`/proxies` endpoint).
   - Filters candidate nodes by allowed country prefixes and prioritizes preferred node names.
   - Conducts concurrent latency probing across candidate nodes.
   - Switches the active selector and executes multi-round post-selection healthchecks.
   - Preserves and rolls back to the previous active node if selection or health checks fail.

---

## Environment Variables Reference

All settings can be configured via environment variables or command-line flags.

### `healthcheck.py`

| Variable | Default | Description | Example (Documentation Safe) |
|---|---|---|---|
| `PROXY_URL` | `None` (Direct) | HTTP/HTTPS proxy URL for egress requests. | `http://127.0.0.1:7890` |
| `EXPECTED_EGRESS_IP` | `None` (Disabled) | Expected public IP address. Verified against `IP_CHECK_URL`. | `198.51.100.1` |
| `IP_CHECK_URL` | `https://api.ipify.org` | Endpoint used to retrieve current egress IP address. | `https://checkip.example.com/` |
| `GOOGLE_CHECK_URL` | `https://www.google.com/generate_204` | Connectivity check URL for Google network. | `https://google-check.example.com/generate_204` |
| `OAUTH_CHECK_URL` | `https://accounts.google.com` | Connectivity check URL for OAuth identity providers. | `https://oauth.example.com/healthz` |
| `CLOUD_CODE_CHECK_URL` | `https://cloudcode.googleapis.com` | Connectivity check URL for developer tooling upstreams. | `https://cloudcode.example.com/v1/status` |
| `CHECK_TIMEOUT` / `TIMEOUT_SECONDS` | `5.0` | Timeout per HTTP probe in seconds. | `5.0` |
| `REPEAT_CHECKS` / `REPEAT_COUNT` | `1` | Number of test rounds performed during a check run. | `3` |
| `FAILURE_THRESHOLD` | `1` | Maximum allowable failed rounds before declaring healthcheck failure. | `1` |

### `select_node.py`

| Variable | Default | Description | Example (Documentation Safe) |
|---|---|---|---|
| `NODE_API_BASE_URL` | `http://127.0.0.1:9090` | Local proxy controller API base URL. | `http://127.0.0.1:9090` |
| `NODE_API_TOKEN` | `None` | Optional Bearer authorization token for local API. | `example_token_placeholder` |
| `SELECTOR_NAME` | `GLOBAL` | Selector proxy group name to inspect and update. | `GLOBAL` |
| `ALLOWED_COUNTRY_PREFIX` | `None` (All allowed) | Comma-separated country code prefixes (e.g. `US,HK,JP`). | `US,HK,JP` |
| `PREFERRED_NODE_NAMES` | `None` | Comma-separated list of prioritized node name substrings. | `Fast,Premium,Direct` |
| `PROBE_CONCURRENCY` | `5` | Maximum worker threads for parallel latency delay probes. | `5` |
| `PROBE_TIMEOUT_MS` | `3000` | Latency probe timeout in milliseconds. | `3000` |
| `DELAY_TEST_URL` | `https://www.google.com/generate_204` | Target URL used for controller latency measurements. | `https://delay-check.example.com/generate_204` |
| `POST_SELECT_HEALTH_ROUNDS` | `2` | Required consecutive healthy rounds after selecting a node. | `2` |
| `POST_SELECT_HEALTH_INTERVAL` | `0.5` | Interval in seconds between post-selection verification rounds. | `0.5` |

---

## Redacted Reason Codes

Outputs and logs use clean, standardized status codes with credentials, URLs, and tokens fully redacted:

| Code | Type | Description |
|---|---|---|
| `OK` | Healthcheck | All service checks and IP match evaluations passed. |
| `ERR_IP_MISMATCH` | Healthcheck | Current egress IP does not match `EXPECTED_EGRESS_IP`. |
| `ERR_HTTP_STATUS` | Healthcheck | One or more check endpoints returned non-2xx/3xx HTTP status. |
| `ERR_TIMEOUT` | Healthcheck | HTTP probe timed out. |
| `ERR_PROXY_CONN` | Healthcheck | Proxy refused connection or failed transport handshake. |
| `ERR_DNS_RESOLUTION`| Healthcheck | Domain name resolution failed. |
| `ERR_NETWORK` | Healthcheck | Socket network or transport error. |
| `ERR_THRESHOLD_EXCEEDED`| Healthcheck | Cumulative failures exceeded `FAILURE_THRESHOLD`. |
| `SELECT_OK` | Node Selection | Healthy candidate node probed, selected, and verified. |
| `ERR_NO_CANDIDATES` | Node Selection | No nodes matched `ALLOWED_COUNTRY_PREFIX`. |
| `ERR_PROBE_FAILED` | Node Selection | Latency probing failed for all candidate nodes. |
| `ERR_POST_CHECK_FAILED` | Node Selection | Candidate failed multi-round post-selection verification. |
| `ROLLBACK_SUCCESS` | Node Selection | Selection failed; active node rolled back to initial state. |
| `ROLLBACK_FAILED` | Node Selection | Selection failed; rollback request to original node failed. |
| `ERR_API_UNAVAILABLE`| Node Selection | Local proxy controller API could not be reached. |

---

## Execution Examples

### CLI Execution

```bash
# Basic health check
python3 healthcheck.py --json

# Run healthcheck with proxy and expected IP
python3 healthcheck.py \
  --proxy-url "http://127.0.0.1:7890" \
  --expected-ip "198.51.100.1" \
  --repeat 3 \
  --threshold 1 \
  --json

# Select best node from allowed country prefix
python3 select_node.py \
  --api-base-url "http://127.0.0.1:9090" \
  --selector "GLOBAL" \
  --allowed-prefix "US,HK" \
  --preferred "Fast,Premium" \
  --health-rounds 2 \
  --json
```

---

## Systemd Service & Timer Templates

The following templates are provided under `ops/egress/`:

- `egress-healthcheck.service` & `egress-healthcheck.timer`: Executes periodic egress health checks.
- `egress-node-select.service` & `egress-node-select.timer`: Executes periodic optimal node selection with rollback protection.
- `egress.env.example`: Template for `/etc/default/egress-proxy.env`.

### Installation Steps

1. Copy environment template to system configuration:
   ```bash
   sudo cp ops/egress/egress.env.example /etc/default/egress-proxy.env
   sudo chmod 600 /etc/default/egress-proxy.env
   ```

2. Copy unit files to systemd directory:
   ```bash
   sudo cp ops/egress/egress-*.service /etc/systemd/system/
   sudo cp ops/egress/egress-*.timer /etc/systemd/system/
   sudo systemctl daemon-reload
   ```

3. Enable and start timers:
   ```bash
   sudo systemctl enable --now egress-healthcheck.timer
   sudo systemctl enable --now egress-node-select.timer
   ```
