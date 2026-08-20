# gateway.yaml schema reference

Single config file at `~/.config/baseten-switch/gateway.yaml` (or
`BASETEN_SWITCH_CONFIG_PATH`) controls the local gateway. `baseten-switch config init`
generates the default file (identical to `gateway.example.yaml`: door
`127.0.0.1:45271` forwarding to router `127.0.0.1:45272`); the gateway
reads it on startup and on
SIGHUP. The native Mac app edits the same file through `baseten-switch`.

## Top-level keys

| Key | Type | Default | Description |
|---|---|---|---|
| `global` | object | (required) | Defaults inherited by all harnesses. |
| `clients` | list[object] | `[]` | Per-harness override blocks. Usually one port per entry; entries may share a `bind_addr` for single-port dispatch (see below). |
| `door` | object | (absent) | Front-door (`baseten-switch door`) port map, making gateway.yaml the single source of truth for the whole request path. |

## `global`

| Key | Type | Default | Description |
|---|---|---|---|
| `routing_enabled` | bool | `true` in new configs | The one global routing gate. It must be explicitly present. Off is absolute: native requests use the protocol-native provider, and aliases, raw Baseten slugs, Baseten model mappings, dedicated Baseten subagents, fallback, and Baseten credentials are not consulted by request resolution. Saved policy remains editable and becomes active again when On. |
| `auth` | map[route] -> secret ref | (empty) | Environment fallback credentials per upstream route. The selected Baseten CLI profile supplies its OAuth or API-key credential automatically. `${VAR}` resolves from env or `~/.config/baseten-switch/env`. |
| `telemetry_dir` | string | `~/.config/baseten-switch/telemetry` | Private directory containing versioned, monthly JSONL request segments. |
| `telemetry_enabled` | bool | `true` | Toggle per-request telemetry collection. Disabling collection preserves existing history. |
| `telemetry_retention_days` | int | `90` | Retention window for closed telemetry segments. The active segment is never deleted. |
| `trace_capture` | object | (absent, disabled) | Separate high-sensitivity local request/response body capture. It never inherits telemetry settings. See below. |
| `retry_max` | int | `3` | Gateway-level retry count against upstream failures. |
| `request_timeout` | duration | `600s` | Hard per-request timeout (Go duration syntax). |
| `ttft_timeout` | duration | (disabled) | Time-to-first-byte deadline per upstream attempt (Go duration syntax; absent or `0` disables). Expiry before a response begins permits the configured fallback; a final attempt or explicit model choice returns 504. The deadline never interrupts an active stream. Telemetry records `fallback_trigger: "ttft_timeout"`. Malformed or negative values refuse the config at load. Start with `30s` and tune for your workload. Per-client `ttft_timeout` overrides this value. |

### `global.trace_capture`

This feature is disabled when the block is absent or `enabled` is false. When
enabled, Switch stores exact client request bodies and gateway-emitted response
bytes in a separate private JSONL store. Captured data can contain prompts,
responses, visible reasoning, tool definitions, tool arguments and results,
source code, files, terminal and MCP output, images, documents, pasted
credentials, personal data, and regulated data. File permissions are not
encryption. Enable capture only under an approved local data-handling policy.

```yaml
global:
  trace_capture:
    enabled: true
    clients:
      - claude-code
    retention_days: 7
```

| Key | Type | Default | Description |
|---|---|---|---|
| `enabled` | bool | `false` | Enables future capture for the exact allowlisted clients. Existing files are preserved when disabled. |
| `clients` | list[string] | `[]` | Required and nonempty when enabled. Every name must resolve to an enabled client whose listener, and any front door targeting it, is bound to loopback. |
| `retention_days` | int | `7` | Retention for closed daily trace segments. Accepted range is 1 through 365 days. |

Trace capture is independent of metadata telemetry. It stores no generic
request or response headers. It is never enabled by an environment variable or
remote provider setting.

### `clients[].model_options`

Reasoning policy is client-scoped, provider-scoped, and keyed by the final
Baseten canonical model:

```yaml
clients:
  - name: claude-code
    model_options:
      baseten:
        zai-org/GLM-5.2:
          reasoning:
            mode: follow_harness
```

`reasoning.mode` accepts:

| Mode | Optional fields | Effect |
|---|---|---|
| `off` | none | Apply the selected protocol adapter's validated Off encoding on the Baseten attempt. |
| `follow_harness` | none | Preserve or translate the harness's semantic reasoning choice when the active protocol adapter supports it. |
| `fixed` | `effort` (required) | Force the exact catalog-advertised effort value. |

Configuration loading validates structure without network access. It rejects
unknown providers, empty model IDs, unknown modes, `fixed` without `effort`,
and `effort` on another mode. Typed CLI writes also validate the choice against
the running gateway's models.dev-derived catalog and per-client adapter
projection.

`global.model_options` is not accepted. Model options are client-scoped.

An absent override uses the runtime safe default. Every model whose catalog
semantic capability and the selected client's protocol adapter intersect on a
validated Off control defaults to Off. A model without a validated Off control
defaults to passthrough, which may leave provider reasoning on. Therefore the
canonical `gateway.example.yaml` intentionally omits `model_options`. Users add
an override only to follow harness reasoning or choose a catalog-supported
fixed effort.

Available modes depend on both the selected model's catalog capabilities and
the client's protocol. The UI exposes only controls that the active
combination supports. Claude Messages supports Off and `follow_harness` for
catalog models with a reasoning toggle, but not categorical effort values.
Other protocols expose exact catalog efforts only when they can encode them.

Resetting a model removes its explicit override and recomputes the safe
default. The full app labels this action `Reset to Safe Default`. A model with
no editable control and default passthrough is a valid read-only provider
default, not a configuration error.

## `clients[]`

Each client entry maps one harness to one local bind address.

| Key | Type | Default | Description |
|---|---|---|---|
| `name` | string | (required) | Harness identifier. Shown in the UI as the card title. |
| `enabled` | bool | `true` | Disabled clients are skipped at config resolve time: the router never binds their port, so the front door serves its hardcoded native fallback for that harness. Their saved mappings and `fallback_route` are dormant intent that takes effect on re-enable. |
| `bind_addr` | string | (required) | `host:port` the gateway listens on for this harness. Clients may share an address if their `protocol_shape`s differ: the gateway binds once and resolves the client per request from the path (anthropic shape owns `/v1/messages*`; openai shape owns `/v1/chat/completions` and `/v1/responses`; `/v1/models` disambiguates on the `anthropic-version` header). Two same-shape clients on one address is a config error: the later one is logged and skipped. |
| `protocol_shape` | enum: `anthropic`,`openai` | `anthropic` | Wire shape the harness sends to the gateway. It selects the listener handler and native provider: `/v1/messages` and Anthropic for `anthropic`, `/v1/chat/completions` and OpenAI for `openai`. An anthropic listener with `upstream_shape: openai` uses cross-shape translation for Baseten traffic. The reverse translation is unsupported. |
| `auth_token` | object | (empty) | Incoming-auth: what the harness sends to the gateway. |
| `default_model` | baseten slug | (required for enabled clients) | Baseten target for requests that do not match an explicit `model_routes` family mapping. Must contain `/`. Disabled clients may omit it while parked. |
| `model_aliases` | map[alias id] -> baseten slug | (empty) | Anthropic-shape clients only. Publishes picker-visible Baseten models to Claude Code's gateway model discovery: the gateway synthesizes them into `GET /v1/models` (Anthropic list shape, `?limit` respected), and Claude Code launched with `CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1` shows them in the `/model` picker. Alias ids must begin with `claude` or `anthropic` (the picker drops everything else before caching) and must not shadow real Anthropic model names (`claude-opus-*`, `claude-sonnet-*`, `claude-haiku-*`, `claude-instant-*`, `claude-<digit>*`); violations refuse the config at load. While global routing is On, a request naming an alias or raw Baseten slug is an explicit Baseten choice with no fallback. While Off, it fails locally with guidance to select a native model or turn routing On; no Baseten credential or endpoint is accessed. Unknown gateway aliases remain a loud error. |
| `subagent_model` | string | (empty) | Anthropic-shape clients only. Rewrite target for Claude Code subagent requests. When global routing is On, the rewrite runs before the normal explicit-choice, family-mapping, and default-mapping ladder. When Off, saved subagent configuration is inactive and the original native model passes through. |
| `subagent_routing` | enum: `on`,`off` | (absent) | Anthropic-shape clients only. Live toggle for the `subagent_model` rewrite, so the menubar can flip it without losing the configured model. `on` enables the rewrite; `off` disables it (sidechain traffic passes untouched, exact factory behavior). Absent means `on` when `subagent_model` is set, so the field exists purely as the off switch. Enabled = `subagent_model` non-empty and `subagent_routing` is not `off`. Validation at load: a value other than empty/`on`/`off`, or `subagent_routing` set while `subagent_model` is empty, is a config-load error. The `baseten-switch claude subagents on|off` verb flips this field then SIGHUPs the gateway; live sessions pick it up on their next sidechain request. |
| `model_routes` | map[family]string | GLM-5.2 for fable, opus, sonnet, and haiku in new configs | Anthropic-shape per-family mappings. Keys are exactly `fable`, `opus`, `sonnet`, or `haiku`. `native` selects Anthropic; a configured alias or raw Baseten slug selects Baseten. A missing family uses the client's `default_model`. While global routing is Off, every mapping is dormant but remains editable. Baseten mappings retain the protocol-native fallback; native mappings do not activate fallback. |
| `model_options` | map[provider] -> map[canonical model ID] -> model options | (empty) | Client-scoped provider/model policy keyed by the final routed model. Version 1 accepts only provider `baseten` and the `reasoning` option above. The policy applies when this client's default, family mapping, alias, raw slug, or subagent override selects that model. |
| `sanitize_history` | bool | `true` | Repair replayed history before forwarding to an anthropic-shape upstream: strip empty or whitespace-only text blocks and normalize `tool_use` and `tool_result` ids to `^[a-zA-Z0-9_-]+$` (harnesses replay ids like `functions.Bash:0` after a route through an OpenAI-shape provider). OpenAI-shape traffic and the monitor route are never touched. |
| `upstream_shape` | enum: `anthropic`,`openai` | (listener shape) | Wire shape used for Baseten traffic. `openai` on an anthropic listener translates `/v1/messages` traffic to `/v1/chat/completions` and re-encodes the response, including streaming, to the anthropic shape. Translation drops thinking blocks and rejects image or document content with a 400. Telemetry rows carry `translated: true`. |
| `responses_compatibility` | object | (absent, all rules `off`) | OpenAI-shape clients only. Configures isolated Responses API safeguards for Baseten attempts. A present block receives the per-rule defaults below; omitting the entire block keeps every rule off. Unknown fields and modes refuse the config. Changes replace the resolved policy on SIGHUP and participate in the listener config hash. |
| `responses_strip_tool_types` | list[string] | (empty) | OpenAI-shape clients only; the field on an Anthropic-shape client refuses the config at load. Tool types listed here are stripped from `tools[]` before a Baseten `/v1/responses` attempt. An object-form `tool_choice` referencing a stripped type is rewritten; string forms like `auto` are untouched. A native fallback keeps the original body byte-for-byte. Telemetry and stderr logs report stripping. Empty entries are invalid, list order is preserved, and a list change respawns the listener on SIGHUP. |
| `ttft_timeout` | duration | inherit `global.ttft_timeout` | Per-harness override of the first-byte deadline (see `global.ttft_timeout` for semantics). `0` disables it for this harness even when the global value is set. |
| `fallback_route` | enum: `anthropic`,`openai` | protocol-native in new configs | Accepts only `NativeRoute(protocol_shape)` or an absent value. It is eligible after default or mapped Baseten policy fails, but is inactive for global Off, native mappings, and explicit alias or raw-slug choices. Anthropic-shape 429 responses relay to the harness without fallback or cooldown; OpenAI-shape 429 responses remain fallback-eligible. |

### `responses_compatibility`

The Codex example enables request normalization and stream consistency
safeguards. The `additional_tools_input` rule remains off unless explicitly
enabled. Each rule is either on or off. Enabled request rules run before the
first upstream attempt and never inspect or retry error bodies.

| Key | Type | Default in a present block | Description |
|---|---|---|---|
| `text_format_default` | enum: `on`,`off` | `on` | Fills an omitted `text.format` with the OpenAI plain-text default. Explicit formats are preserved. |
| `additional_tools_input` | enum: `on`,`off` | `off` | Experimental. When explicitly enabled, hoists one `additional_tools` input item into top-level `tools`. The rewrite can change custom-tool semantics, so keep it off unless validating that compatibility path. Ambiguous definitions fail open and increment compatibility validation telemetry. |
| `reasoning_effort` | enum: `on`,`off` | `on` | Normalizes recognized reasoning effort values to the supported public set. Unknown values pass through unchanged. |
| `function_arguments_consistency` | enum: `on`,`off` | `on` | Checks streaming function-argument completion consistency and repairs incomplete completion events. Correct events remain byte-for-byte unchanged. |

### `auth_token`

| Key | Type | Description |
|---|---|---|
| `header` | enum: `Authorization`,`x-api-key` | Which request header the harness sends its credential in. Claude Code sends `Authorization: Bearer …`; raw SDK clients may use `x-api-key`. |
| `value` | string (secret ref) | `${VAR}` style env reference (resolved from `~/.config/baseten-switch/env` then process env). Compared against incoming header value; mismatch returns 401. |

## `door`

Configures the `baseten-switch door` front-door process from the same file, replacing
its launch flags (flags still win when given, as a test/emergency
override). The door re-reads this section on SIGHUP and diffs port specs:
unchanged ports keep serving, removed ports shut down, new ports bind.

| Key | Type | Default | Description |
|---|---|---|---|
| `cooldown` | duration | `15s` | How long a tripped port serves the native fallback before re-probing the router. |
| `probe_interval` | duration | `3s` | Health-probe cadence against the router while tripped. |
| `ports[]` | list[object] | `[]` | Harness-facing ports. |
| `ports[].bind_addr` | string | (required) | `host:port` the door listens on (what harness env points at). |
| `ports[].router_addr` | string | (required) | The router listener behind this port; must match one or more clients' `bind_addr`. Fallback hosts are derived per path from those clients' shapes. |

## `${VAR}` substitution

Anywhere a field has type "secret ref", the value `${VAR_NAME}` is
substituted at gateway startup from:
1. process environment (set when the gateway was started)
2. `~/.config/baseten-switch/env` (mode 0600, KEY=VALUE lines)

Resolved values are never written back to disk. The Tauri UI never
displays resolved secrets; it shows the `${VAR_NAME}` placeholder
verbatim and lets the user edit only the variable name.

### Baseten authentication precedence

Switch reads the selected profile created by `baseten auth login`. A selected
OAuth profile or API-key profile takes precedence over `global.auth.baseten`.
API keys remain opaque; Baseten enforces their permissions and resource
access.

`global.auth.baseten` expands to `BASETEN_API_KEY`, which is a separate
environment fallback. Switch uses it only when
`BASETEN_SWITCH_API_KEY_FALLBACK=1` and the selected CLI profile has no usable
credential. Store both values in the mode-0600 Switch environment file when
you need this fallback:

```text
BASETEN_SWITCH_API_KEY_FALLBACK=1
BASETEN_API_KEY=...
```

`GET /v1/admin/auth/status` and the `auth` block in `GET /v1/admin/status`
report `auth_type` as `oauth`, `api_key`, or `none`. An environment-only
credential reports `auth_type: api_key` with `signed_in: false` and
`fallback_in_use: true`; it does not become a selected CLI profile.

## Reload semantics

- Gateway starts: one-shot full parse of gateway.yaml.
- Signal: SIGHUP triggers a full reload. Active in-flight requests
  finish against the old config; new requests use the new config.
- Telemetry enablement, directory, and retention changes apply on reload.
  Disabling collection closes the active writer without deleting history;
  enabling collection opens the writer lazily on the next completed request.
- The native app invokes typed `baseten-switch` mutations. Routing mutations
  serialize writers, compare the exact config hash and active router token,
  journal the transaction, then confirm activation after SIGHUP. The app does
  not rewrite YAML directly. A direct external edit is visible as a
  desired-versus-active mismatch and must not be silently overwritten.

## Reasoning policy commands

The typed provider/model commands are:

```sh
baseten-switch claude reasoning baseten zai-org/GLM-5.2 off
baseten-switch claude reasoning baseten zai-org/GLM-5.2 follow-harness
baseten-switch codex reasoning baseten deepseek-ai/DeepSeek-V4-Pro effort high
baseten-switch claude reasoning baseten zai-org/GLM-5.2 default
```

`effort <value>` stores `mode: fixed`. `default` implements Reset to Safe
Default: it removes the model-specific override, prunes empty parent mappings,
and lets the runtime recompute the catalog-and-adapter safe default. It is
offline-safe because removal needs no catalog lookup.

`off`, `follow-harness`, and `effort` require a healthy router. The gateway
validates the choice against one catalog snapshot and the selected client's
adapter. It does not inspect or warn about another client.

All four commands use the config lock, exact config hash, transaction journal,
SIGHUP, and bounded activation confirmation used by other typed mutations.

## Config reset

`baseten-switch config reset --yes` replaces the active configuration with
the canonical template. It first writes a unique adjacent
`gateway.yaml.pre-reset-*.bak` containing the exact previous bytes.
Unsupported keys are rejected rather than silently ignored.

## Forward-compat with new harnesses (opencode, codex)

Each harness is one `clients[]` entry. Adding a new harness means
adding a validated client configuration through a supported configuration
workflow, then reloading the router. The current native app exposes Claude
configuration only. Future product controls should use sibling harness
namespaces such as `baseten-switch codex ...`, not a generic client-routing layer
or a direct YAML editor. When a harness needs gateway-side transformation
(for example, different SSE chunk boundaries or embedded base64 tool output),
the dispatch is selected by `name` inside the proxy layer; new transformation
hooks ship as new code paths while the config schema remains stable.
