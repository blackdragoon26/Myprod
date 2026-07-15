# Managed Netlify DNS

Myprod can create and verify an application's public A record from the hosted
dashboard. The browser never receives a DNS credential. The Oracle-local agent
calls Netlify and stores only sanitized status in the pool state.

## One-Time Oracle Setup

1. In Netlify, open **User settings -> Applications -> Personal access
   tokens** and create a dedicated token for Myprod. Rotate it on a defined
   operator schedule.
2. SSH to Oracle:

   ```sh
   ssh -i ~/.ssh/keys/openclaw-oracle.key ubuntu@140.245.5.201
   ```

3. Open the root-readable agent environment with an editor. `sudoedit` avoids
   putting the token in shell history:

   ```sh
   sudoedit /etc/poolctl-agent.env
   ```

4. Preserve the existing `POOLCTL_AGENT_TOKEN` line and add:

   ```txt
   NETLIFY_AUTH_TOKEN=<paste the dedicated Netlify token here>
   MYPROD_DNS_ZONE=sankalpjha.dev
   MYPROD_INGRESS_IPV4=140.245.5.201
   ```

5. Save, then enforce permissions and restart only the agent:

   ```sh
   sudo chown root:root /etc/poolctl-agent.env
   sudo chmod 600 /etc/poolctl-agent.env
   sudo systemctl restart poolctl-agent
   sudo systemctl is-active poolctl-agent
   ```

6. Unlock the hosted dashboard and refresh. **Add application** should show
   that Netlify DNS is configured for `sankalpjha.dev` and targets
   `140.245.5.201`.

Do not paste the token into chat, browser forms, application fields, Git, or
command output. Do not run a command that prints `/etc/poolctl-agent.env`.

## Application Flow

1. Select **Add application** and enter a subdomain such as
   `splidt-api.sankalpjha.dev`.
2. Keep **Create and verify DNS automatically** enabled.
3. Select **Register application** and accept the DNS-specific confirmation.
4. Myprod finds the Netlify zone and lists records for the exact hostname.
5. If no conflicting record exists, it creates an A record with a 300-second
   TTL pointing to `140.245.5.201`.
6. The agent checks public resolution. The app shows `ready` immediately when
   resolved or `pending` while propagation is incomplete.
7. For `pending`, wait and select **Check DNS**. Deployment is enabled only
   after the managed record reaches `ready`.

Registration never starts the application. **Deploy** remains a separate,
confirmed Nomad mutation.

## State Meanings

- `manual`: DNS is owned outside Myprod; deployment is not DNS-gated.
- `pending`: the Netlify record exists but public resolution is not yet proven.
- `ready`: the hostname resolves to the configured Oracle ingress IPv4.
- `conflict`: an A, AAAA, or CNAME record already exists with another target.
- `error`: Netlify or resolver verification failed.
- `unconfigured`: Oracle is missing a valid token, zone, or IPv4 target.

Myprod does not overwrite or delete conflicts. Inspect the record in Netlify,
decide which target is correct, and make any destructive edit manually. It also
does not manage the zone apex, wildcard records, IPv6, CNAME, MX, TXT, or other
DNS providers.

## Read-Only Verification

After configuration or an agent deployment:

1. Confirm `poolctl-agent` is active.
2. Unlock and refresh the hosted dashboard.
3. Confirm the add-application dialog reports the expected zone and ingress IP.
4. For a managed app, confirm its DNS badge and message match public resolution:

   ```sh
   dig +short A splidt-api.sankalpjha.dev
   ```

5. Expect `140.245.5.201` before deploying.

## Credential Rotation Or Removal

Create a replacement Netlify token, update only `NETLIFY_AUTH_TOKEN` with
`sudoedit`, restart `poolctl-agent`, and verify the dashboard capability before
revoking the old token.

To disable automation, remove the three managed-DNS variables with `sudoedit`
and restart the agent. Existing DNS records remain untouched. Applications
already marked for managed DNS cannot deploy until automation is restored and
the record is verified; Myprod deliberately does not silently convert them to
manual ownership.
