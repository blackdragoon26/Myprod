# Operator Authentication

The hosted dashboard uses Clerk for human operator sign-in. The production
application is invite-only, with email one-time codes enabled. The current
operator invitation is restricted to the owner's Clerk account.

Application secrets remain outside Clerk and the dashboard. CI deploy tokens
remain app-scoped credentials minted and hash-stored by Myprod. These are three
separate credential classes:

- Clerk sessions authenticate a human operator to the dashboard;
- Myprod-issued deploy tokens authenticate one repository to one app endpoint;
- application secrets are installed directly on the target node under
  `/etc/poolctl/apps`.

## Browser Flow

`GET /api/auth-config` returns only Clerk's publishable key and derived Frontend
API hostname. It never returns `CLERK_SECRET_KEY`. The static page loads ClerkJS
from that first-party Frontend API and presents a focused custom flow: enter the
invited email, receive a one-time code, verify the six digits, and finalize the
Clerk session. Password authentication is deliberately omitted from Myprod's
UI even if it is enabled elsewhere in the Clerk instance. The browser sends a
short-lived session JWT in `Authorization: Bearer` for cross-origin calls to
the Oracle agent. Session tokens are kept by Clerk and are not copied to local
storage.

The legacy `POOLCTL_AGENT_TOKEN` remains available through **Recovery token**.
It is a break-glass path during migration and Clerk outages, not the ordinary
sign-in flow. It must remain configured on Oracle until Clerk login has been
verified end to end from a fresh browser.

## Agent Verification

The agent accepts either the recovery operator token or a verified Clerk
session. Clerk verification is enabled only when all three variables below are
present in Oracle's root-readable `/etc/poolctl-agent.env`:

```txt
POOLCTL_CLERK_ISSUER=https://clerk.sankalpjha.dev
POOLCTL_CLERK_AUTHORIZED_PARTIES=https://control.sankalpjha.dev,https://myprod-control.vercel.app
POOLCTL_CLERK_OPERATOR_USER_IDS=user_replace_with_verified_clerk_id
```

`POOLCTL_CLERK_OPERATOR_USER_IDS` contains immutable Clerk user IDs, not email
addresses. Resolve the ID from Clerk's production user list after the invited
user completes the first sign-in. Never guess it and never use an invitation
ID or session ID in its place.

The verifier requires all of the following:

- an RSA-SHA256 signature from the configured issuer's public JWKS;
- the exact issuer;
- an unexpired token with a valid issued-at time;
- a non-empty Clerk session ID;
- an exact authorized-party origin;
- an exact subject match in the operator user-ID allowlist.

JWKS keys are public, fetched over HTTPS, cached for 15 minutes, and refreshed
when Clerk presents an unknown key ID. No Clerk secret key is installed on
Oracle.

## Compatibility-First Rollout

1. Deploy the agent binary while leaving the three Clerk variables unset. The
   recovery token remains the only accepted human credential.
2. Deploy the dashboard. Clerk sign-in becomes visible, but recovery access
   remains available.
3. Complete the invited user's first production sign-in and obtain the exact
   production `user_...` ID from Clerk.
4. Back up `/etc/poolctl-agent.env`, install the three variables with
   `sudoedit`, and restart only `poolctl-agent`.
5. Confirm `clerkOperatorAuthV1` in authenticated status, sign in from a fresh
   browser, and exercise a read-only refresh before any mutation.
6. Verify the recovery token still works from a separate browser profile.

This rollout never restarts or resubmits Nomad application jobs. Restarting the
control agent does not stop existing backend allocations.

## Passkeys and Touch ID

Touch ID on macOS is exposed to web applications through passkeys/WebAuthn.
Clerk currently requires a paid plan for passkey support on this application.
Email OTP is therefore the active primary method. Do not enable or purchase a
paid Clerk plan without explicit operator approval. Once enabled, passkeys can
appear as an alternative sign-in factor without changing the Oracle JWT
verification path.
