# Local Isolated Protocol Verification Guide

This guide describes how to run reproducible, secret-free, isolated local verifications for CLIProxyAPI's native non-chat endpoints, Usage accounting, egress health detection, and node selection without accessing or modifying production environments.

---

## 1. Principles

1. **Zero Production Access**: Never access production credentials, `.env`, `auths/` directory, production Nginx, or live server instances.
2. **Dynamic Ephemeral Ports**: All mock services and local test proxies bind to ephemeral OS ports (`127.0.0.1:0`), avoiding port collisions.
3. **Isolated Mock Providers**: Use in-memory `httptest.Server` or local mock HTTP servers with temporary tokens and dummy payload generators.
4. **Credential Isolation**: Client authorization headers are verified to never leak to upstream providers.
5. **Error Desensitization**: Upstream failures containing internal URLs, proxy IPs, keys, or file paths are scrubbed before returning to the client.
6. **Strict Usage Accounting**: No tokens are fabricated for non-token metrics (such as audio seconds, image counts, or video tasks).

---

## 2. Capabilities Verified

| Capability | Standard Endpoint | Upstream Path | Metric (Billing Unit) | Verification Focus |
|---|---|---|---|---|
| **Embedding** | `POST /v1/embeddings` | `/embeddings` | `tokens` | Input tokens counted, alias remapped |
| **Rerank** | `POST /v1/rerank` | `/rerank` | `tokens` | Document scoring, alias remapped |
| **ASR** | `POST /v1/audio/transcriptions` | `/audio/transcriptions` | `audio_seconds` | Audio duration counted, 0 tokens fabricated |
| **TTS** | `POST /v1/audio/speech` | `/audio/speech` | `characters` | Binary MP3 frame returned intact, char count |
| **Image** | `POST /v1/images/generations` | `/images/generations` | `images` | Image object count, 0 tokens fabricated |
| **Video Submit** | `POST /v1/video/submit` | `/video/submit` | `video_tasks` | Returns `requestId`, bills 1 task |
| **Video Status** | `POST /v1/video/status` | `/video/status` | `video_status` | Status polling does **not** duplicate task billing |

---

## 3. Running Automated Go Integration Tests

Execute the comprehensive end-to-end isolated verification suite:

```bash
# Run isolated end-to-end verification covering all 6 capabilities
go test -v -run "^TestIsolatedEndToEndAllSixCapabilities$" ./test

# Run native capability unit tests (17 test cases covering forwarding, alias, 401/429/500, desensitization)
go test -v -run "TestNativeImage|TestNativeVideo|TestNativeSpeech" ./sdk/api/handlers/openai

# Run non-chat Usage event accounting tests
go test -v -run "^TestUsageEvent" ./sdk/api/handlers/openai
```

---

## 4. Running Python Egress & Node Selection Tests

All egress proxy detection scripts run using the standard Python 3 runtime with zero third-party dependencies:

```bash
# Run multi-round healthcheck and dynamic node selection tests
python3 -m unittest ops/egress/test_egress.py

# Test node selection in dry-run mode (probes candidates without switching active node)
python3 ops/egress/select_node.py --dry-run --json
```

---

## 5. Security Invariants Enforced

- **Authorization Isolation**: Client `Authorization: Bearer <client_key>` header is explicitly removed before forwarding to upstreams.
- **Provider Key Isolation**: Upstream `Authorization` is strictly derived from provider configuration (`api-key-entries`).
- **Data Scrubbing**: Upstream errors (HTTP 401, 403, 404, 429, 500) scrub all occurrences of:
  - Internal Base URLs and hostnames
  - Upstream API keys
  - Local filesystem paths (`/var/...`, `/app/...`, `/home/...`, Windows paths)
  - Internal Provider names
  - Internal proxy IP addresses (`10.x`, `192.168.x`, `172.16-31.x`, `127.0.0.1`, `localhost`)
- **SSE Non-Buffering**: All streaming responses include `X-Accel-Buffering: no` to disable intermediate reverse proxy buffering.
- **Request Body Protection**: Enforces a 64MB body size limit on incoming requests via `http.MaxBytesReader` to guard against denial-of-service and memory exhaustion.
