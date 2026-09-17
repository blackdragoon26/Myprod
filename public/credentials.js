/* Operator-only, write-only credential forms. Values never enter snapshots,
   localStorage, the app form, or Agent Output. */
(() => {
  const registryButton = document.getElementById("registry-connections");
  const dialog = document.createElement("dialog");
  dialog.className = "token-dialog credential-dialog";
  dialog.setAttribute("aria-labelledby", "credential-title");
  dialog.innerHTML = `<div class="app-form-head"><h2 id="credential-title"></h2><button type="button" data-close aria-label="Close">×</button></div>
    <div data-content></div><p data-message role="status" style="white-space:pre-wrap"></p>
    <div class="form-actions"><button type="button" data-close>Done</button></div>`;
  document.body.append(dialog);
  const content = dialog.querySelector("[data-content]");
  const message = dialog.querySelector("[data-message]");
  let generation = 0;
  let busy = false;
  let appName = "";
  let metadata = null;
  let connections = [];
  const el = (id) => dialog.querySelector(`#${id}`);
  function close() {
    generation++;
    content.querySelectorAll("input,textarea").forEach((field) => { field.value = ""; });
    content.replaceChildren(); message.textContent = ""; appName = ""; metadata = null;
    if (dialog.open) dialog.close();
  }
  window.closeCredentialDialogs = close;
  dialog.querySelectorAll("[data-close]").forEach((button) => button.addEventListener("click", close));
  dialog.addEventListener("cancel", (event) => { event.preventDefault(); close(); });
  function show(title) { close(); el("credential-title").textContent = title; dialog.showModal(); }
  async function run(operation) {
    if (busy || !agentConnected) return;
    busy = true; const current = generation;
    setActionBusy(true);
    content.querySelectorAll("button").forEach((b) => { b.disabled = true; });
    try { await operation(current); }
    catch (error) { if (current === generation) message.textContent = error.message; }
    finally { busy = false; setActionBusy(false); if (current === generation) content.querySelectorAll("button").forEach((b) => { b.disabled = false; }); }
  }
  function stillOpen(current) { return dialog.open && generation === current; }
  async function loadConnections() {
    const data = await agentFetch("/registries");
    return data.registries || [];
  }
  async function openApp(name) {
    if (!agentConnected || !latestCapabilities.appSecretsV1) return;
    show(`Secrets & registry · ${name}`); appName = name;
    await run(async (current) => {
      const [data, list] = await Promise.all([agentFetch(`/apps/${encodeURIComponent(name)}/credentials`), loadConnections()]);
      if (!stillOpen(current)) return;
      metadata = data.credentials; connections = list; renderApp();
    });
  }
  function renderApp() {
    const app = (latestConfig.Apps || []).find((entry) => entry.Name === appName);
    content.innerHTML = `<p>Secrets are supplied as environment variables when you apply this configuration. Saving a draft leaves running apps unchanged. Ordinary Deploy and CI use the last applied version.</p>
      ${app?.SecretEnv ? `<p class="form-note">This app uses a legacy secrets file. To use managed environment secrets, disable that mount in Edit first. Existing files are preserved.</p>` : ""}
      <p><strong>${metadata.pending ? "Pending changes" : metadata.active ? "Applied" : "No managed credentials"}</strong>${metadata.active ? ` · Version ${escapeHTML(metadata.active.id.slice(0, 8))}` : ""}</p>
      <div id="credential-keys">${metadata.keys.map((key) => `<label style="display:flex;align-items:center;gap:8px;padding:4px 0"><input type="checkbox" data-remove-key="${escapeHTML(key)}"> Remove <code>${escapeHTML(key)}</code></label>`).join("") || "No secret keys saved."}</div>
      <div class="field"><label for="credential-values">Add or replace secrets</label><textarea id="credential-values" rows="5" autocomplete="off" spellcheck="false" placeholder="API_KEY=value&#10;DATABASE_URL=value"></textarea><p>One NAME=value per line. Values are literal: quotes are preserved; no shell expansion. Existing values are never displayed. Unmentioned keys are kept.</p></div>
      <div class="field"><label for="credential-registry">Image registry connection</label><select id="credential-registry"><option value="">No managed connection (public or existing node setup)</option>${connections.map((c) => `<option value="${escapeHTML(c.name)}">${escapeHTML(c.name)} · ${escapeHTML(c.host)}</option>`).join("")}</select><p>Add or rotate connections from Registry connections above the app list.</p></div>
      <div class="form-actions"><button type="button" id="credential-save">Save draft</button><button type="button" id="credential-apply">Apply & restart app</button>${metadata.previous ? '<button type="button" id="credential-rollback">Restore previous credentials</button>' : ""}</div>`;
    el("credential-registry").value = metadata.registry;
    el("credential-save").addEventListener("click", () => run(saveDraft));
    el("credential-apply").addEventListener("click", () => apply("apply"));
    el("credential-rollback")?.addEventListener("click", () => apply("rollback"));
  }
  async function saveDraft(current) {
    const values = Object.create(null);
    for (const line of el("credential-values").value.split("\n")) {
      if (!line.trim()) continue;
      const equals = line.indexOf("=");
      if (equals < 1) throw new Error("Each secret must use NAME=value.");
      const name = line.slice(0, equals).trim();
      if (Object.hasOwn(values, name)) throw new Error("Duplicate secret name. Keep one value per name.");
      values[name] = line.slice(equals + 1);
    }
    content.querySelectorAll("[data-remove-key]:checked").forEach((field) => { values[field.dataset.removeKey] = null; });
    const body = JSON.stringify({ revision: metadata.revision, values, registry: el("credential-registry").value });
    const name = appName;
    const data = await agentFetch(`/apps/${encodeURIComponent(name)}/credentials`, { method: "PUT", body });
    if (!stillOpen(current)) return;
    el("credential-values").value = "";
    metadata = data.credentials; renderApp(); message.textContent = "Draft saved. Apply & restart app when ready.";
  }
  async function apply(operation) {
    if (el("credential-values").value || content.querySelector("[data-remove-key]:checked") || el("credential-registry").value !== metadata.registry) {
      message.textContent = "Save the draft before applying or restoring credentials."; return;
    }
    const name = appName;
    if (!window.confirm(`${operation === "rollback" ? "Restore previous credentials for" : "Apply saved credentials to"} ${name}? This deploys the current app configuration and replaces only this app's allocation. Brief downtime is possible.`)) return;
    await run(async (current) => {
      message.textContent = "Deploying and checking health…";
      const result = await agentFetch(`/apps/${encodeURIComponent(name)}/credentials/${operation}`, { method: "POST" });
      const data = await agentFetch(`/apps/${encodeURIComponent(name)}/credentials`);
      if (!stillOpen(current)) return;
      metadata = data.credentials; renderApp(); message.textContent = result.output;
      await refreshAgent({ silent: true });
    });
  }
  async function openRegistries() {
    if (!agentConnected || !latestCapabilities.registryConnectionsV1) return;
    show("Registry connections");
    await run(async (current) => { const list = await loadConnections(); if (!stillOpen(current)) return; connections = list; renderRegistries(); });
  }
  function renderRegistries() {
    content.innerHTML = `<p>Save a GHCR pull credential once and select it for each app. Use a GitHub personal access token (classic) with <code>read:packages</code> and authorize organization SSO if required. Secrets are encrypted in Nomad and never displayed again.</p>
      <div class="field"><label for="registry-select">Connection</label><select id="registry-select"><option value="">New connection</option>${connections.map((c) => `<option value="${escapeHTML(c.name)}">${escapeHTML(c.name)}</option>`).join("")}</select></div>
      <div class="field"><label for="registry-name">Connection name</label><input id="registry-name" maxlength="64" autocomplete="off" placeholder="github-backends"></div>
      <div class="field"><label for="registry-user">GitHub username</label><input id="registry-user" maxlength="128" autocomplete="off"></div>
      <div class="field"><label for="registry-password">New pull token</label><input id="registry-password" type="password" autocomplete="new-password" maxlength="8192"></div>
      <div class="form-actions"><button type="button" id="registry-save">Save connection</button><button type="button" id="registry-delete">Delete connection</button></div>
      <div class="field"><label for="registry-image">Test access to an image</label><input id="registry-image" autocomplete="off" placeholder="ghcr.io/owner/backend@sha256:…"></div><button type="button" id="registry-test">Test saved connection</button>
      <p>Rotation does not restart apps. Apply credentials separately for each app to use the new token. Deleting a connection does not revoke the token at GitHub.</p>`;
    el("registry-select").addEventListener("change", () => {
      const c = connections.find((entry) => entry.name === el("registry-select").value);
      el("registry-name").value = c?.name || ""; el("registry-name").disabled = Boolean(c);
      el("registry-user").value = c?.username || ""; el("registry-password").value = "";
    });
    el("registry-save").addEventListener("click", () => run(async (current) => {
      const name = el("registry-name").value.trim();
      if (!name) throw new Error("Enter a connection name.");
      const existing = connections.find((c) => c.name === name);
      await agentFetch(`/registries/${encodeURIComponent(name)}`, { method: "PUT", body: JSON.stringify({ host: "ghcr.io", username: el("registry-user").value.trim(), password: el("registry-password").value, revision: existing?.revision || "" }) });
      if (!stillOpen(current)) return;
      el("registry-password").value = "";
      const list = await loadConnections(); if (!stillOpen(current)) return;
      connections = list; renderRegistries(); message.textContent = "Connection saved. Select it in the app's Secrets & registry screen, then apply.";
    }));
    el("registry-delete").addEventListener("click", () => run(async (current) => {
      const name = el("registry-select").value; if (!name) throw new Error("Select a saved connection.");
      if (!window.confirm(`Delete registry connection ${name}? Connections referenced by app drafts or active/previous versions cannot be deleted.`)) return;
      await agentFetch(`/registries/${encodeURIComponent(name)}`, { method: "DELETE" });
      const list = await loadConnections(); if (!stillOpen(current)) return;
      connections = list; renderRegistries(); message.textContent = "Connection deleted.";
    }));
    el("registry-test").addEventListener("click", () => run(async (current) => {
      const name = el("registry-select").value; if (!name) throw new Error("Select a saved connection.");
      message.textContent = "Checking GHCR access…";
      const result = await agentFetch(`/registries/${encodeURIComponent(name)}/test`, { method: "POST", body: JSON.stringify({ image: el("registry-image").value.trim() }) });
      if (stillOpen(current)) message.textContent = result.output;
    }));
  }
  registryButton.addEventListener("click", openRegistries);
  window.bindCredentialButtons = () => {
    registryButton.hidden = !latestCapabilities.registryConnectionsV1;
    registryButton.disabled = !agentConnected || actionRunning;
    document.querySelectorAll("[data-app-credentials]").forEach((button) => {
      button.disabled = !agentConnected || actionRunning;
      if (button.dataset.bound) return;
      button.dataset.bound = "true"; button.addEventListener("click", () => openApp(button.dataset.appCredentials));
    });
  };
  window.bindCredentialButtons();
})();
