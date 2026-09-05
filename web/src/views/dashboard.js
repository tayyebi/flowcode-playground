import { api, withAdminRetry } from "../api.js";
import { promptForAdminToken } from "./adminPrompt.js";

const STARTER_FILE = `workflow: HelloWorld

step greeting:
    emit
        value = "hello, world"
end
`;

export async function mount(container) {
  container.innerHTML = "";

  const header = document.createElement("header");
  header.className = "dash-header";
  header.innerHTML = `<h1>Projects</h1><p class="dash-sub">Saved, versioned FlowCode workspaces — deploy them as web apps, schedule them, and inspect their run history.</p>`;
  container.append(header);

  const form = document.createElement("form");
  form.className = "dash-create";
  form.innerHTML = `
    <input type="text" name="name" placeholder="Project name" required maxlength="200" />
    <input type="text" name="description" placeholder="Description (optional)" maxlength="500" />
    <button type="submit" class="btn btn-primary">New project</button>
  `;
  container.append(form);

  const status = document.createElement("p");
  status.className = "note note-muted";
  container.append(status);

  const list = document.createElement("div");
  list.className = "project-list";
  container.append(list);

  async function refresh() {
    list.innerHTML = "";
    status.textContent = "Loading…";
    try {
      const projects = await withAdminRetry(() => api.listProjects(), promptForAdminToken);
      status.textContent = "";
      if (projects.length === 0) {
        list.append(emptyState());
        return;
      }
      for (const p of projects) list.append(projectCard(p, refresh));
    } catch (err) {
      status.textContent = `Could not load projects: ${err.message}`;
      status.className = "note note-error";
    }
  }

  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    const data = new FormData(form);
    const name = data.get("name")?.toString().trim();
    if (!name) return;
    try {
      const project = await withAdminRetry(
        () => api.createProject(name, data.get("description")?.toString().trim() ?? ""),
        promptForAdminToken,
      );
      // A brand-new project otherwise has zero files — nothing to run,
      // deploy, or trigger — so seed one starter file, mirroring how Apps
      // Script always gives a new project a default Code.gs to start from.
      await withAdminRetry(() => api.saveFile(project.id, "main.fc", STARTER_FILE), promptForAdminToken);
      form.reset();
      window.location.hash = `#/projects/${project.id}`;
    } catch (err) {
      status.textContent = `Could not create project: ${err.message}`;
      status.className = "note note-error";
    }
  });

  await refresh();
}

function emptyState() {
  const p = document.createElement("p");
  p.className = "note note-muted";
  p.textContent = "No projects yet. Create one above to get started.";
  return p;
}

function projectCard(project, onChange) {
  const card = document.createElement("article");
  card.className = "project-card";

  const link = document.createElement("a");
  link.href = `#/projects/${project.id}`;
  link.className = "project-card-name";
  link.textContent = project.name;

  const meta = document.createElement("p");
  meta.className = "project-card-meta";
  meta.textContent = project.description || "No description";

  const updated = document.createElement("p");
  updated.className = "project-card-updated";
  updated.textContent = `Updated ${project.updatedAt}`;

  const del = document.createElement("button");
  del.type = "button";
  del.className = "btn btn-secondary btn-danger";
  del.textContent = "Delete";
  del.addEventListener("click", async () => {
    if (!window.confirm(`Delete "${project.name}"? This removes all its files, versions, deployments, and triggers.`)) return;
    await withAdminRetry(() => api.deleteProject(project.id), promptForAdminToken);
    onChange();
  });

  card.append(link, meta, updated, del);
  return card;
}
