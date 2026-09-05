import { EditorView, keymap, lineNumbers, highlightActiveLine } from "@codemirror/view";
import { EditorState, Compartment } from "@codemirror/state";
import { defaultKeymap, history, historyKeymap, indentWithTab } from "@codemirror/commands";
import { bracketMatching, indentUnit } from "@codemirror/language";
import { linter, lintGutter, setDiagnostics } from "@codemirror/lint";

import { flowcodeLanguage, flowcodeHighlighting } from "../flowcode-lang.js";
import { renderTrace, renderBytecode, renderDiagnosticsList, statusFromResult, note } from "../render-result.js";
import { api, withAdminRetry } from "../api.js";
import { promptForAdminToken } from "./adminPrompt.js";
import { mountManagePanels } from "./project-manage.js";

const NEW_FILE_TEMPLATE = `workflow: NewWorkflow

step first:
    emit
        value = "hello"
end
`;

export async function mount(container, route) {
  const projectId = route.projectId;
  container.innerHTML = "";

  let project, files;
  try {
    [project, files] = await withAdminRetry(
      () => Promise.all([api.getProject(projectId), api.listFiles(projectId)]),
      promptForAdminToken,
    );
  } catch (err) {
    container.append(note(`Could not load project: ${err.message}`, "error"));
    return;
  }

  const state = { activeFile: files[0]?.name ?? null, dirty: new Set() };

  container.append(
    backLink(),
    projectHeader(project),
  );

  const workspace = document.createElement("section");
  workspace.className = "project-workspace";
  container.append(workspace);

  const tabStrip = document.createElement("div");
  tabStrip.className = "file-tabs";
  workspace.append(tabStrip);

  const editorPane = document.createElement("div");
  editorPane.className = "pane pane-editor";
  const editorHost = document.createElement("div");
  editorHost.id = "project-editor";
  editorPane.append(editorHost);
  workspace.append(editorPane);

  const resultsPane = document.createElement("section");
  resultsPane.className = "pane pane-results";
  resultsPane.innerHTML = `
    <div class="tabs" role="tablist">
      <button role="tab" class="tab" data-tab="trace" aria-selected="true">Trace</button>
      <button role="tab" class="tab" data-tab="bytecode" aria-selected="false">Bytecode</button>
      <button role="tab" class="tab" data-tab="diagnostics" aria-selected="false">Diagnostics</button>
    </div>
    <div class="panels">
      <div class="panel" data-panel="trace"></div>
      <div class="panel" data-panel="bytecode" hidden></div>
      <div class="panel" data-panel="diagnostics" hidden></div>
    </div>
    <div class="statusbar" role="status" aria-live="polite"></div>
  `;
  workspace.append(resultsPane);

  const panels = {
    trace: resultsPane.querySelector('[data-panel="trace"]'),
    bytecode: resultsPane.querySelector('[data-panel="bytecode"]'),
    diagnostics: resultsPane.querySelector('[data-panel="diagnostics"]'),
  };
  const statusEl = resultsPane.querySelector(".statusbar");
  for (const tab of resultsPane.querySelectorAll(".tab")) {
    tab.addEventListener("click", () => selectResultTab(resultsPane, panels, tab.dataset.tab));
  }

  const toolbar = document.createElement("div");
  toolbar.className = "project-toolbar";
  toolbar.innerHTML = `
    <button type="button" class="btn btn-secondary" data-action="new-file">New file</button>
    <button type="button" class="btn btn-secondary" data-action="delete-file">Delete file</button>
    <span class="spacer"></span>
    <span class="save-state" data-save-state></span>
    <button type="button" class="btn btn-secondary" data-action="save-version">Save version…</button>
    <button type="button" class="btn btn-primary" data-action="run">Run <kbd>⌘↵</kbd></button>
  `;
  tabStrip.before(toolbar);

  let currentDiagnostics = [];
  const themeCompartment = new Compartment();
  const view = new EditorView({
    parent: editorHost,
    state: EditorState.create({
      doc: state.activeFile ? files[0].content : "",
      extensions: [
        lineNumbers(),
        history(),
        bracketMatching(),
        highlightActiveLine(),
        lintGutter(),
        indentUnit.of("    "),
        flowcodeLanguage,
        themeCompartment.of(flowcodeHighlighting(window.matchMedia?.("(prefers-color-scheme: dark)").matches ?? false)),
        EditorView.lineWrapping,
        linter(() => currentDiagnostics, { delay: 0 }),
        EditorView.updateListener.of((update) => {
          if (update.docChanged && state.activeFile) {
            state.dirty.add(state.activeFile);
            renderTabStrip();
            renderSaveState();
          }
        }),
        keymap.of([
          { key: "Mod-Enter", run: () => (runActive(), true) },
          { key: "Mod-s", run: () => (saveActive(), true) },
          indentWithTab,
          ...defaultKeymap,
          ...historyKeymap,
        ]),
      ],
    }),
  });

  function fileByName(name) {
    return files.find((f) => f.name === name);
  }

  function renderTabStrip() {
    tabStrip.innerHTML = "";
    for (const f of files) {
      const tab = document.createElement("button");
      tab.type = "button";
      tab.className = "file-tab" + (f.name === state.activeFile ? " active" : "");
      tab.textContent = f.name + (state.dirty.has(f.name) ? " •" : "");
      tab.addEventListener("click", () => switchTo(f.name));
      tabStrip.append(tab);
    }
  }

  function switchTo(name) {
    if (state.activeFile) {
      fileByName(state.activeFile).content = view.state.doc.toString();
    }
    state.activeFile = name;
    const f = fileByName(name);
    view.dispatch({
      changes: { from: 0, to: view.state.doc.length, insert: f.content },
      selection: { anchor: 0 },
    });
    renderTabStrip();
  }

  function renderSaveState() {
    const el = toolbar.querySelector("[data-save-state]");
    el.textContent = state.dirty.size > 0 ? "Unsaved changes" : "";
  }

  async function saveActive() {
    if (!state.activeFile) return;
    const content = view.state.doc.toString();
    fileByName(state.activeFile).content = content;
    try {
      await withAdminRetry(() => api.saveFile(projectId, state.activeFile, content), promptForAdminToken);
      state.dirty.delete(state.activeFile);
      renderTabStrip();
      renderSaveState();
      setStatus("Saved.", "ok");
    } catch (err) {
      setStatus(`Could not save: ${err.message}`, "error");
    }
  }

  async function runActive() {
    if (!state.activeFile) return;
    await saveActive();
    setStatus("Running…", "busy");
    try {
      const result = await withAdminRetry(() => api.runFile(projectId, state.activeFile), promptForAdminToken);
      currentDiagnostics = (result.compile.diagnostics ?? []).map((d) => {
        const line = view.state.doc.line(Math.min(Math.max(d.line, 1), view.state.doc.lines));
        return { from: line.from, to: line.to, severity: d.level === "error" ? "error" : "warning", message: d.message };
      });
      view.dispatch(setDiagnostics(view.state, currentDiagnostics));
      renderDiagnosticsList(panels.diagnostics, result.compile.diagnostics ?? [], result.compile, {
        onJumpToLine: (n) => {
          const line = view.state.doc.line(Math.min(Math.max(n, 1), view.state.doc.lines));
          view.dispatch({ selection: { anchor: line.from }, effects: EditorView.scrollIntoView(line.from, { y: "center" }) });
          view.focus();
        },
      });
      renderBytecode(panels.bytecode, result);
      renderTrace(panels.trace, result);
      const { message, kind } = statusFromResult(result);
      setStatus(message, kind);
      selectResultTab(resultsPane, panels, result.compile.exitCode !== 0 ? "diagnostics" : "trace");
      manage.refreshExecutions();
    } catch (err) {
      setStatus(`Could not run: ${err.message}`, "error");
    }
  }

  function setStatus(message, kind) {
    statusEl.textContent = message;
    statusEl.className = `statusbar${kind ? ` statusbar-${kind}` : ""}`;
  }

  toolbar.querySelector('[data-action="run"]').addEventListener("click", runActive);
  toolbar.querySelector('[data-action="new-file"]').addEventListener("click", async () => {
    const name = window.prompt("New file name (e.g. main.fc):");
    if (!name) return;
    try {
      const f = await withAdminRetry(() => api.saveFile(projectId, name, NEW_FILE_TEMPLATE), promptForAdminToken);
      files.push(f);
      switchTo(f.name);
    } catch (err) {
      setStatus(`Could not create file: ${err.message}`, "error");
    }
  });
  toolbar.querySelector('[data-action="delete-file"]').addEventListener("click", async () => {
    if (!state.activeFile || files.length <= 1) {
      window.alert("A project needs at least one file.");
      return;
    }
    if (!window.confirm(`Delete ${state.activeFile}? Any deployment or trigger using it will also be removed.`)) return;
    await withAdminRetry(() => api.deleteFile(projectId, state.activeFile), promptForAdminToken);
    files = files.filter((f) => f.name !== state.activeFile);
    switchTo(files[0].name);
    manage.refreshFileOptions(files);
  });
  toolbar.querySelector('[data-action="save-version"]').addEventListener("click", async () => {
    await saveActive();
    const label = window.prompt("Label for this version (optional):", "") ?? "";
    try {
      const v = await withAdminRetry(() => api.createVersion(projectId, label), promptForAdminToken);
      setStatus(`Saved version ${v.number}.`, "ok");
      manage.refreshVersions();
    } catch (err) {
      setStatus(`Could not save version: ${err.message}`, "error");
    }
  });

  renderTabStrip();
  renderSaveState();
  panels.trace.append(note("Press Run, or ⌘↵, to compile and execute this file.", "muted"));

  const manageSection = document.createElement("section");
  manageSection.className = "project-manage";
  container.append(manageSection);
  const manage = mountManagePanels(manageSection, projectId, files);

  return () => view.destroy();
}

function backLink() {
  const a = document.createElement("a");
  a.href = "#/dashboard";
  a.className = "back-link";
  a.textContent = "← All projects";
  return a;
}

function projectHeader(project) {
  const header = document.createElement("header");
  header.className = "project-header";
  header.innerHTML = `<h1>${escapeHtml(project.name)}</h1>${project.description ? `<p class="project-desc">${escapeHtml(project.description)}</p>` : ""}`;
  return header;
}

function selectResultTab(root, panels, name) {
  for (const tab of root.querySelectorAll(".tab")) {
    tab.setAttribute("aria-selected", String(tab.dataset.tab === name));
  }
  for (const [key, panel] of Object.entries(panels)) {
    panel.hidden = key !== name;
  }
}

function escapeHtml(s) {
  return s.replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
}
