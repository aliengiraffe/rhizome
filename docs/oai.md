# Experimental/internal: Sign in with ChatGPT

These hidden commands provide an authentication foundation for future LLM
features. They are not part of setup or the public CLI. Nothing in MCP, the
observer, gateway, history, configuration, or updates reads these credentials.
There is one local account registration; no API key, device-code, SSH sign-in,
peer forwarding, or uninstall integration is provided.

## Usage

```sh
rhizome auth oai
rhizome auth oai status
rhizome test oai
rhizome test oai --model <slug> --prompt 'Reply with a short greeting.'
rhizome auth oai logout
```

Sign-in opens a system browser with a five-minute deadline. If opening fails,
copy the URL printed to stderr into a browser on the same device. Ctrl-C
cancels the listener. The browser page does not indicate success: the terminal
reports the verified account, granted plan permission, and access-token expiry.
A successful identity sign-in without `chatgpt.tokens.use.direct` does not
permit inference. Explicitly sign in again to request consent.

`status` reads an atomic metadata snapshot only: it makes no network requests,
does not access the OS credential store, and does not refresh or write files.
It reports the last verified local state, not whether remote access has since
been revoked or whether the OS store is currently unlocked.

`test oai` defaults to the first model with `visibility == "list"` in the
account's server-ordered `/v1/models` response. `--model` skips that lookup.
The default prompt asks for a one-sentence hello from Rhizome. Requests use
an array of user messages, `store: false`, and `stream: true`. The reply is
printed only after `response.completed`, with a 1 MiB text bound. Partial,
failed, empty, and interrupted responses exit unsuccessfully. This is a real
request against the user's eligible ChatGPT plan; check
[ChatGPT usage](https://chatgpt.com/settings/usage) for usage and permissions.
No retries or redirects are performed, including on HTTP 429.

Logout attempts remote revocation, removes local token records, and retains
registration. If remote revocation cannot be confirmed, it says so; disconnect
the app in ChatGPT settings. A locked or unavailable OS store can prevent local
deletion: unlock it and rerun logout. Do not delete metadata before logout,
because it identifies the OS records to remove.

## Storage and failure recovery

Non-secret state is `~/.rhizome/oai/state.json`, inside a private `0700`
directory with atomic `0600` replacement (current-user DACLs on Windows).
It includes the stable host UUID, client ID, latest loopback port, verified
subject/account label, scopes, expiry, and opaque credential-record references.
It is separate from `config.toml` and does not follow `runtime_dir` overrides.
Writers serialize with an OS process lock; process exit releases the lock.

Access, refresh and ID tokens are stored only in:

- macOS Keychain, using `/usr/bin/security`; secret values travel over stdin,
  never argv. Native stdout/stderr is never forwarded.
- Linux Secret Service via `secret-tool` (install your distribution's libsecret
  tools and run an unlocked Secret Service session).
- Windows Credential Manager, using the pure-Go
  [`github.com/danieljoos/wincred`](https://github.com/danieljoos/wincred) module.

There is no plaintext fallback. Immutable records use bounded chunks to fit
native credential limits. Metadata is switched only after every chunk is
stored; obsolete or interrupted records remain tracked for cleanup. Platform
command invocations have a 15-second deadline. No DevKit code or assets are
vendored, translated, or bundled.

Refresh starts within 60 seconds of expiry and respects `earliest_refresh_at`.
The token response's missing refresh `scope` preserves the previous grant.
Rotated credentials are committed before verifying a returned ID token; pending
credentials cannot authorize a request. A transient JWKS failure leaves this
pending record available for verification on the next invocation, without
reusing the consumed refresh token. A changed subject fails with
`account_mismatch`. An interrupted or ambiguous refresh, malformed token
response, or failed rotation write leaves a persisted fence requiring a new
`auth oai`, rather than risking reuse of a consumed token. Terminal refresh
errors clear unusable tokens while preserving registration.

Discovery requires an exact issuer and same-origin, TLS endpoints. ID tokens
require an advertised supported algorithm (RS256 or ES256), matching JWKS key,
valid signature, issuer, audience/authorized party, expiry, and sign-in nonce.
The callback accepts only GET on its exact `127.0.0.1:<port>/auth/callback`,
rejects duplicate parameters, and compares state in constant time. Invalid
state does not cancel the pending attempt. Every HTTP operation has a deadline
and bounded reads: 1 MiB JSON, 8 MiB SSE, 256 KiB per event. Remote errors expose
only known error codes, HTTP status, and bounded request IDs; raw server text,
tokens, and prompts are not printed. Reply text is intentionally printed by
`test oai`.

## Protocol sources and decisions

Implemented from the public OAuth/OIDC documentation, checked November 12, 2026:

- [Registration and sign-in](https://developers.openai.com/siwc/token-sharing-open-source/sign-in)
- [Token reference](https://developers.openai.com/siwc/token-sharing-open-source/token-reference)
- [Accounts and sessions](https://developers.openai.com/siwc/token-sharing-open-source/profiles-and-sessions)
- [Models and inference](https://developers.openai.com/siwc/token-sharing-open-source/models-and-inference)
- [Errors and recovery](https://developers.openai.com/siwc/token-sharing-open-source/errors-and-recovery)
- [Preview limitations](https://developers.openai.com/siwc/token-sharing-open-source/preview-limitations)

The current docs supersede three details in the original issue's observations:
the stable `ext_agent_host_id` is required; a registration may use a different
loopback port on later sign-ins; and a returning callback may omit `client_id`.
The exact URI still matches within each attempt, and a supplied differing
client ID is rejected. First registration is persisted before code exchange.
The optional `id_token_hint` is deliberately omitted so tokens never enter
URLs, browser argv, or fallback URL output. Single-account operation and
OS-only secret storage follow this issue's narrower scope.

## Verification

Automated tests use synthetic RSA/ECDSA keys, local fake issuer/API servers,
and an in-memory credential store. They do not access real accounts or native
credential stores. Run them in disposable user state as described in
[AGENTS.md](../AGENTS.md): `go test -race ./internal/oai` and the root CLI tests,
then the repository build/check and required smoke scripts.

Live verification record: **not performed**. No real sign-in, real inference,
plan-usage observation, or native OS-store round trip has been claimed.
macOS and Linux interactive verification remain pending; Windows is covered
by compilation only when the cross-build check passes.

Manual checklist on a disposable OS user/account:

1. Run `auth oai`, finish browser consent, then `status`; verify the account,
   plan usage and expiry. Check owner-only metadata permissions.
2. Inspect the state schema and process arguments locally: no access, refresh,
   or ID token should be present. Never paste tokens or transcripts into an issue.
3. Run the default test and explicit model/prompt overrides. Confirm a non-empty
   reply and corresponding usage in ChatGPT settings.
4. After expiry, confirm renewal and repeat status/inference. Interrupt a sign-in
   and check the listener exits. With the credential store locked/unavailable,
   confirm failure without a plaintext fallback.
5. Logout, confirm `test oai` fails cleanly, and sign in again to the same
   registration. Record OS, date, pass/fail, and any sanitized request ID here.
