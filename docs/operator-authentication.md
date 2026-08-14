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

The production static page currently loads ClerkJS v6 directly. Its custom
email-code implementation supports Clerk's legacy browser-resource sequence:

1. create a sign-in with the email identifier;
2. select the supported `email_code` first factor;
3. prepare that factor with its `emailAddressId`;
4. attempt the factor with the six-digit code;
5. call `Clerk.setActive` with the resulting session ID.

Do not assume the React `SignInFuture` methods exist on the direct browser SDK.
In particular, do not add an unconditional `signIn.reset()` or
`signIn.emailCode.sendCode()` call. Retain the feature-detected compatibility
path in `public/index.html`, and test the actual production button after a hard
reload whenever the Clerk SDK URL or auth flow changes.

Reference: [Clerk legacy email/SMS OTP custom flow](https://clerk.com/docs/guides/development/custom-flows/authentication/legacy/email-sms-otp).

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
addresses. Resolve the ID from Clerk's production user list. Never guess it and
never use an invitation ID or session ID in its place.

## Provisioning An Invited Operator

A Clerk invitation and a Clerk user are separate resources. Sending an
invitation does not guarantee that direct email-OTP sign-in will find an
account. Use one of these paths:

1. Have the operator accept the invitation link and finish account creation.
2. With explicit operator authorization, create the passwordless user through
   Clerk's Backend API using the already-configured production secret key.

For the second path, first query the production user list by exact email and
create only when no match exists. Supply `email_address` and
`skip_password_requirement: true`. Clerk marks email addresses created by the
Backend API as verified. Record only the returned immutable `user_...` ID; do
not print, persist, or copy `CLERK_SECRET_KEY` into the repository, Oracle, a
command log, or chat. Retrieve the key into a mode-restricted temporary file,
perform the request, and delete that file immediately.

Reference: [Clerk Backend API `createUser`](https://clerk.com/docs/reference/backend/user/create-user).

After provisioning, exercise the dashboard in this order:

1. Hard-reload <https://control.sankalpjha.dev/>.
2. Request an email code and confirm the UI advances to the six-digit form.
3. Verify the code and confirm Clerk creates an active browser session.
4. Refresh agent state read-only before attempting any mutation.

`Couldn't find your account` means the browser integration is working but the
Clerk user does not exist in the production instance. A JavaScript method error
such as `signIn.reset is not a function` means the custom UI is calling an API
that the loaded ClerkJS build does not expose.

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
3. Ensure the invited production user exists, then obtain the exact production
   `user_...` ID from Clerk.
4. Back up `/etc/poolctl-agent.env`, install the three variables with
   `sudoedit`, and restart only `poolctl-agent`.
5. Confirm `clerkOperatorAuthV1` in authenticated status, sign in from a fresh
   browser, and exercise a read-only refresh before any mutation.
6. Verify the recovery token still works from a separate browser profile.

This rollout never restarts or resubmits Nomad application jobs. Restarting the
control agent does not stop existing backend allocations.

Before restarting `poolctl-agent`, back up both `/usr/local/bin/poolctl` and
`/etc/poolctl-agent.env`. Install the Linux ARM64 binary atomically and attach a
rollback that restores both backups if the service or local health route does
not recover. Afterward verify all of the following:

- `poolctl-agent`, Nomad, Traefik, and `wg-quick@wg0` are active;
- authenticated status advertises `managedAppLifecycleV2`,
  `appDeployTokensV1`, and `clerkOperatorAuthV1`;
- the recovery credential still authenticates;
- `https://control.sankalpjha.dev/api/smoke` reports both HTTP and HTTPS healthy;
- existing Nomad jobs were neither registered nor restarted by the rollout.

## Passkeys and Touch ID

Touch ID on macOS is exposed to web applications through passkeys/WebAuthn.
Clerk currently requires a paid plan for passkey support on this application.
Email OTP is therefore the active primary method. Do not enable or purchase a
paid Clerk plan without explicit operator approval. Once enabled, passkeys can
appear as an alternative sign-in factor without changing the Oracle JWT
verification path.
