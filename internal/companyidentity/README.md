# Company employee checks

The existing central directory remains the employee source. This client reads
its authenticated, read-only member projection; it does not sync a second
employee list, create WeKnora users, or match accounts by email/name.

Deployment configuration:

| Variable | Meaning |
| --- | --- |
| `COMPANY_IDENTITY_STATUS_ORIGIN` | Private central status service origin, reachable from the app network. No public proxy or redirect. |
| `COMPANY_IDENTITY_STATUS_KEY_FILE` | Read-only mount of the existing 32-byte status key, with mode 0400/0600 and no group/other access. |
| `COMPANY_IDENTITY_ISSUER` | Stable issuer, defaults to `https://auth.ch-chip.com`. |
| `COMPANY_IDENTITY_KNOWLEDGE_BASE_IDS` | Explicit comma-separated approved library IDs; empty is rejected. |
| `COMPANY_IDENTITY_REQUIRE_EMPLOYEE_MCP` | `true` rejects unbound legacy MCP credentials on this deployment. |
| `COMPANY_IDENTITY_ALLOW_ORIGINAL_DOWNLOADS` | Explicit original-file entitlement, default false. |
| `COMPANY_KNOWLEDGE_DOWNLOAD_BASE_URL` | Company SSO portal base, e.g. `https://auth.ch-chip.com/company/knowledge`. |

The status service must deploy `/internal/wecom-member-status` alongside its
existing `/internal/member-status`. Requests use the existing canonical HMAC
protocol, unique nonces and path/body binding. No positive cache is used. A
disabled employee, ambiguous platform ID, directory outage or invalid reply
fails closed. Status responses and credentials are not logged.

MCP endpoints accept `employee_binding: {"issuer": "...", "sub": "..."}`.
Only an authenticated administrator/service may create an endpoint, and the
company self-service portal supplies the current employee's stable subject.
Knowledge bases must be explicit and inside the deployment allowlist. Employee
endpoints permit only retrieval/wiki tools; `ask` and writes are rejected.
`get_original_document` is additionally gated by the original-file entitlement.
The returned URL carries no credential and checks the employee again when opened.

An optional `weknora_user_id` is an explicit local-account binding. That account
must remain active, retain an active workspace membership and retain access to
every endpoint library. It never provisions or grants membership. An issued
binding cannot be transferred to another employee/account; create a new endpoint.
Token rotation, endpoint disable, central employee disable and local membership
revocation apply to subsequent HTTP requests and tool calls. Existing source
files, model keys and legacy endpoints are not altered by the schema migration;
the deployment switch determines whether unbound endpoints remain usable.

`Checker.LookupWeCom` accepts only the opaque UserID from the authenticated
platform callback and requires exact case. iLink IDs are not website OpenIDs;
they need their own explicit binding before a company channel can authorize them.
