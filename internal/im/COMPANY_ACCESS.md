# Company employee IM channels

Company WeCom and WeChat channels use the existing central employee directory.
They fail closed when the directory or the deployment allowlist is unavailable.
Other platform integrations retain their existing behavior.

Required deployment configuration is shared with employee MCP endpoints:

| Setting | Meaning |
| --- | --- |
| `COMPANY_IDENTITY_STATUS_ORIGIN` | Private company identity status origin |
| `COMPANY_IDENTITY_STATUS_KEY_FILE` | Owner-only file containing the 32-byte HMAC key |
| `COMPANY_IDENTITY_ISSUER` | Stable issuer, normally `https://auth.ch-chip.com` |
| `COMPANY_IDENTITY_KNOWLEDGE_BASE_IDS` | Explicit original-manufacturer KB allowlist; newly created KBs are excluded |
| `COMPANY_IDENTITY_ALLOW_ORIGINAL_DOWNLOADS` | Explicit original delivery grant, default `false` |
| `COMPANY_IM_DOWNLOAD_BASE_URL` | HTTPS employee portal base, for example `https://auth.ch-chip.com/company/knowledge` |

WeCom takes the exact, case-sensitive `from.userid` from the authenticated robot
connection. A callback for another BotID, a non-member compatibility event, an
unknown or inactive employee, and group conversations cannot run company QA.
Use a dedicated BotID or a controlled switch: each BotID supports one active
connection. Starting another client can disconnect the previous service.

WeChat QR codes are generated locally with the existing TDesign QR component.
The QR token is never submitted to a public image-generation service. After
scanning, the administrator explicitly binds the returned `ilink_user_id` to
the employee's portal identity through `employee_issuer` and `employee_subject`
in channel credentials. The IM panel exposes these fields. Website OpenID is
never interpreted as an iLink identifier. `employee_bindings` can alternatively
hold an explicit map from exact iLink sender IDs to `{issuer, sub}` bindings.
Rebinding the QR code clears the old credentials and requires a new identity
binding. Unbound senders cannot search or receive company originals.

Every message and queued execution rechecks employee eligibility. The request
uses full-answer delivery even when a company channel was saved as streaming.
Before a full answer or its fallback is sent, eligibility is checked again;
an employee revoked while QA was running receives no delayed knowledge answer.
This is a delivery-boundary check, not a polling loop. Other IM platforms retain
their selected streaming mode. The request
uses an employee principal, Viewer role, and a read/chat API scope intersected
with the channel agent's KBs. The request clone permits only native knowledge
and attachment read tools; shell, write tools, external MCP services and skills
are disabled for this employee channel. Uploaded chat attachments can be read by QA but
are not automatically written into a company KB. Session mapping includes the
stable employee identity, account creation time and channel.

The native search, directory and document-read tools return a complete
`original_download` link. The model-context resource codec preserves that link
as a single handle so the model does not have to reconstruct a document UUID.
When the agent supplies a real `/platform/source-download/<knowledge-id>` link,
the sender reloads that knowledge entry, checks its tenant and both KB scopes,
rechecks the employee, and opens the original through `GetKnowledgeFile`.
This supports stored files and the existing read-only NAS source references.
Model-generated paths and arbitrary URLs are never opened for original delivery.

Up to five requested originals per answer, each at most 20 MiB, are sent as
native attachments. WeCom uses acknowledged
init/chunk/finish uploads and acknowledged file delivery; WeChat uses encrypted
iLink CDN uploads followed by a checked send result. Larger files retain the
employee portal download link `/<knowledge-id>/download`. The portal must check
the current logged-in employee and the document's KB on every download; the
link does not carry a bearer token or bypass permissions.

Transport tests use local WebSocket/HTTP fakes and preserve PDF/ZIP bytes.
They do not establish a real recipient's receipt or client download. Runtime
acceptance still requires an authorized employee to ask for a real original,
open the native file or authenticated large-file link, and test revocation.
