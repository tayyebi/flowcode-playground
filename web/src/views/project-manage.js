import { api, withAdminRetry } from "../api.js";
import { promptForAdminToken } from "./adminPrompt.js";

// mountManagePanels renders Deployments, Triggers, Executions, and the KV
// debug log for a project, and returns a small controller the file/editor
// toolbar can call into (e.g. after a Run, or after files change).
export function mountManagePanels(container, projectId, files) {
  container.innerHTML = "";

  const nav = document.createElement("div");
  nav.className = "manage-tabs";
  const sections = {};
  for (const [key, label] of Object.entries({
    deployments: "Deployments",
    triggers: "Triggers",
    executions: "Executions",
    kv: "KV store",
  })) {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "manage-tab";
    btn.textContent = label;
    btn.addEventListener("click", () => showSection(key));
    nav.append(btn);
    sections[key] = { button: btn, el: document.createElement("div") };
    sections[key].el.className = "manage-panel";
    sections[key].el.hidden = true;
  }
  container.append(nav);
  for (const s of Object.values(sections)) container.append(s.el);

  function showSection(key) {
    for (const [k, s] of Object.entries(sections)) {
      s.el.hidden = k !== key;
      s.button.classList.toggle("active", k === key);
    }
  }
  sections.deployments.button.classList.add("active");
  sections.deployments.el.hidden = false;

  const refreshDeployments = () => renderDeployments(sections.deployments.el, projectId, files);
  const refreshTriggers = () => renderTriggers(sections.triggers.el, projectId, files);
  const refreshExecutions = () => renderExecutions(sections.executions.el, projectId);
  const refreshKV = () => renderKV(sections.kv.el, projectId);
  const refreshVersions = () => {}; // versions have no standalone panel yet; surfaced via file/version pickers below

  refreshDeployments();
  refreshTriggers();
  refreshExecutions();
  refreshKV();

  return {
    refreshExecutions,
    refreshVersions,
    refreshFileOptions(newFiles) {
      files = newFiles;
      refreshDeployments();
      refreshTriggers();
    },
  };
}

function fileOptions(files, select) {
  select.innerHTML = "";
  for (const f of files) {
    const opt = document.createElement("option");
    opt.value = f.name;
    opt.textContent = f.name;
    select.append(opt);
  }
}

async function renderDeployments(el, projectId, files) {
  el.innerHTML = "";
  const form = document.createElement("form");
  form.className = "manage-create";
  const select = document.createElement("select");
  fileOptions(files, select);
  form.append(select);
  const submit = document.createElement("button");
  submit.type = "submit";
  submit.className = "btn btn-secondary";
  submit.textContent = "Deploy as web app";
  form.append(submit);
  el.append(form);

  const list = document.createElement("div");
  list.className = "manage-list";
  el.append(list);

  async function refresh() {
    list.innerHTML = "";
    const deployments = await withAdminRetry(() => api.listDeployments(projectId), promptForAdminToken);
    if (deployments.length === 0) {
      list.append(muted("No deployments yet. A deployment publishes one file as a public URL."));
      return;
    }
    for (const d of deployments) {
      const row = document.createElement("div");
      row.className = "manage-row";
      const url = `${window.location.origin}/deploy/${d.slug}`;
      row.innerHTML = `
        <a href="${url}" target="_blank" rel="noopener" class="manage-row-title">/deploy/${d.slug}</a>
        <span class="manage-row-meta">${d.fileName}${d.enabled ? "" : " · disabled"}</span>
      `;
      const toggle = document.createElement("button");
      toggle.type = "button";
      toggle.className = "btn btn-secondary";
      toggle.textContent = d.enabled ? "Disable" : "Enable";
      toggle.addEventListener("click", async () => {
        await withAdminRetry(() => api.setDeploymentEnabled(projectId, d.id, !d.enabled), promptForAdminToken);
        refresh();
      });
      const del = document.createElement("button");
      del.type = "button";
      del.className = "btn btn-secondary btn-danger";
      del.textContent = "Delete";
      del.addEventListener("click", async () => {
        await withAdminRetry(() => api.deleteDeployment(projectId, d.id), promptForAdminToken);
        refresh();
      });
      row.append(toggle, del);
      list.append(row);
    }
  }

  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    await withAdminRetry(() => api.createDeployment(projectId, select.value), promptForAdminToken);
    refresh();
  });

  await refresh();
}

async function renderTriggers(el, projectId, files) {
  el.innerHTML = "";
  const form = document.createElement("form");
  form.className = "manage-create";
  const select = document.createElement("select");
  fileOptions(files, select);
  const scheduleSelect = document.createElement("select");
  scheduleSelect.innerHTML = `
    <option value="60">Every minute</option>
    <option value="300">Every 5 minutes</option>
    <option value="900">Every 15 minutes</option>
    <option value="1800">Every 30 minutes</option>
    <option value="3600">Every hour</option>
    <option value="21600">Every 6 hours</option>
    <option value="daily">Daily at...</option>
  `;
  const dailyTime = document.createElement("input");
  dailyTime.type = "time";
  dailyTime.value = "09:00";
  dailyTime.hidden = true;
  scheduleSelect.addEventListener("change", () => {
    dailyTime.hidden = scheduleSelect.value !== "daily";
  });
  const submit = document.createElement("button");
  submit.type = "submit";
  submit.className = "btn btn-secondary";
  submit.textContent = "Add trigger";
  form.append(select, scheduleSelect, dailyTime, submit);
  el.append(form);

  const list = document.createElement("div");
  list.className = "manage-list";
  el.append(list);

  async function refresh() {
    list.innerHTML = "";
    const triggers = await withAdminRetry(() => api.listTriggers(projectId), promptForAdminToken);
    if (triggers.length === 0) {
      list.append(muted("No triggers yet. A trigger runs a file automatically on a schedule."));
      return;
    }
    for (const t of triggers) {
      const row = document.createElement("div");
      row.className = "manage-row";
      const schedule = t.scheduleType === "daily" ? `daily at ${t.dailyTimeUtc} UTC` : `every ${t.intervalSeconds}s`;
      row.innerHTML = `
        <span class="manage-row-title">${t.fileName}</span>
        <span class="manage-row-meta">${schedule}${t.enabled ? "" : " · disabled"} · next run ${t.nextRunAt}</span>
      `;
      const toggle = document.createElement("button");
      toggle.type = "button";
      toggle.className = "btn btn-secondary";
      toggle.textContent = t.enabled ? "Disable" : "Enable";
      toggle.addEventListener("click", async () => {
        await withAdminRetry(() => api.setTriggerEnabled(projectId, t.id, !t.enabled), promptForAdminToken);
        refresh();
      });
      const del = document.createElement("button");
      del.type = "button";
      del.className = "btn btn-secondary btn-danger";
      del.textContent = "Delete";
      del.addEventListener("click", async () => {
        await withAdminRetry(() => api.deleteTrigger(projectId, t.id), promptForAdminToken);
        refresh();
      });
      row.append(toggle, del);
      list.append(row);
    }
  }

  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    const fields = { fileName: select.value };
    if (scheduleSelect.value === "daily") {
      fields.scheduleType = "daily";
      fields.dailyTimeUtc = dailyTime.value || "09:00";
    } else {
      fields.scheduleType = "interval";
      fields.intervalSeconds = Number(scheduleSelect.value);
    }
    await withAdminRetry(() => api.createTrigger(projectId, fields), promptForAdminToken);
    refresh();
  });

  await refresh();
}

async function renderExecutions(el, projectId) {
  el.innerHTML = "";
  const list = document.createElement("div");
  list.className = "manage-list";
  el.append(list);

  async function refresh() {
    list.innerHTML = "";
    const executions = await withAdminRetry(() => api.listExecutions(projectId), promptForAdminToken);
    if (executions.length === 0) {
      list.append(muted("No executions recorded yet."));
      return;
    }
    for (const e of executions) {
      const row = document.createElement("div");
      row.className = "manage-row";
      const failed = (e.compileExitCode ?? 0) !== 0 || (e.runExitCode ?? 0) !== 0;
      row.innerHTML = `
        <span class="manage-row-title">${e.fileName} <span class="tag tag-${e.source}">${e.source}</span></span>
        <span class="manage-row-meta">${e.startedAt} · ${e.durationMs ?? "?"}ms${failed ? " · failed" : ""}</span>
      `;
      list.append(row);
    }
  }

  await refresh();
}

async function renderKV(el, projectId) {
  el.innerHTML = "";
  el.append(
    muted(
      "Best-effort log of “store set” calls parsed from run traces. This is a debug/audit log, " +
        "not a working key-value API a running script can read from yet.",
    ),
  );
  const list = document.createElement("div");
  list.className = "manage-list";
  el.append(list);

  const entries = await withAdminRetry(() => api.listKV(projectId), promptForAdminToken);
  if (entries.length === 0) {
    list.append(muted("No entries recorded yet."));
    return;
  }
  for (const kv of entries) {
    const row = document.createElement("div");
    row.className = "manage-row";
    row.innerHTML = `<span class="manage-row-title">${kv.key}</span><span class="manage-row-meta">${kv.value} · updated ${kv.updatedAt}</span>`;
    const del = document.createElement("button");
    del.type = "button";
    del.className = "btn btn-secondary btn-danger";
    del.textContent = "Delete";
    del.addEventListener("click", async () => {
      await withAdminRetry(() => api.deleteKV(projectId, kv.key), promptForAdminToken);
      row.remove();
    });
    row.append(del);
    list.append(row);
  }
}

function muted(text) {
  const p = document.createElement("p");
  p.className = "note note-muted";
  p.textContent = text;
  return p;
}
