import { EditorView, keymap, lineNumbers, highlightActiveLine } from "@codemirror/view";
import { EditorState, Compartment } from "@codemirror/state";
import { defaultKeymap, history, historyKeymap, indentWithTab } from "@codemirror/commands";
import { bracketMatching, indentUnit } from "@codemirror/language";
import { linter, lintGutter, setDiagnostics } from "@codemirror/lint";

import { flowcodeLanguage, flowcodeHighlighting } from "./flowcode-lang.js";
import { encodeSource, decodeFragment } from "./share.js";
import { renderDiagnosticsList, renderBytecode, renderTrace, statusFromResult, note } from "./render-result.js";
import { initRouter } from "./router.js";
import "./style.css";

const STARTER = `workflow: HelloWorld

step greeting:
    emit
        value = "hello, world"
end

step saved:
    store set
        key = "greeting"
        value = greeting
end
`;

const playgroundRoot = document.getElementById("view-playground");

const el = {
  editor: document.getElementById("editor"),
  run: document.getElementById("run"),
  share: document.getElementById("share"),
  sampleSelect: document.getElementById("sample-select"),
  sampleDescription: document.getElementById("sample-description"),
  status: document.getElementById("status"),
  badge: document.getElementById("diagnostics-badge"),
  panels: {
    trace: document.getElementById("panel-trace"),
    bytecode: document.getElementById("panel-bytecode"),
    diagnostics: document.getElementById("panel-diagnostics"),
  },
};

let samples = [];
let running = false;
// The linter is pull-based, so the latest response is held here for it to read
// rather than being pushed into the editor from the fetch handler.
let currentDiagnostics = [];

/* ------------------------------------------------------------------ */
/* Editor                                                              */
/* ------------------------------------------------------------------ */

const themeCompartment = new Compartment();

const baseTheme = EditorView.theme({
  "&": { height: "100%", fontSize: "13px" },
  ".cm-scroller": {
    fontFamily: "var(--mono)",
    lineHeight: "1.6",
    overflow: "auto",
  },
  ".cm-content": { padding: "12px 0" },
  ".cm-gutters": { border: "none", background: "transparent" },
});

function prefersDark() {
  return window.matchMedia?.("(prefers-color-scheme: dark)").matches ?? false;
}

const view = new EditorView({
  parent: el.editor,
  state: EditorState.create({
    doc: STARTER,
    extensions: [
      lineNumbers(),
      history(),
      bracketMatching(),
      highlightActiveLine(),
      lintGutter(),
      // FlowCode blocks are indented four spaces throughout the samples, and
      // the compiler is whitespace-tolerant but the convention is worth keeping.
      indentUnit.of("    "),
      flowcodeLanguage,
      themeCompartment.of(flowcodeHighlighting(prefersDark())),
      baseTheme,
      EditorView.lineWrapping,
      linter(() => currentDiagnostics, { delay: 0 }),
      keymap.of([
        { key: "Mod-Enter", run: () => (runProgram(), true) },
        // The browser's own Mod-s would offer to save the page, which is never
        // what someone wants here.
        { key: "Mod-s", run: () => (runProgram(), true) },
        indentWithTab,
        ...defaultKeymap,
        ...historyKeymap,
      ]),
    ],
  }),
});

// Follow the OS theme live, so a system switch doesn't leave the editor's
// syntax colours mismatched against the rest of the page.
window.matchMedia?.("(prefers-color-scheme: dark)").addEventListener("change", (e) => {
  view.dispatch({
    effects: themeCompartment.reconfigure(flowcodeHighlighting(e.matches)),
  });
});

function getSource() {
  return view.state.doc.toString();
}

function setSource(text) {
  view.dispatch({
    changes: { from: 0, to: view.state.doc.length, insert: text },
    selection: { anchor: 0 },
  });
}

/* ------------------------------------------------------------------ */
/* Running                                                             */
/* ------------------------------------------------------------------ */

async function runProgram() {
  if (running) return;
  running = true;
  el.run.disabled = true;
  setStatus("Compiling…", "busy");

  try {
    const response = await fetch("/api/run", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ source: getSource() }),
    });

    const body = await response.json().catch(() => null);

    if (!response.ok) {
      // 4xx/5xx mean the request itself was refused — rate limited, too large,
      // server busy. A program that merely fails to compile arrives as a 200.
      const message = body?.error ?? `request failed (${response.status})`;
      showRequestError(message);
      setStatus(message, "error");
      return;
    }

    render(body);
  } catch (err) {
    const message = "Could not reach the playground server.";
    showRequestError(`${message} ${err}`);
    setStatus(message, "error");
  } finally {
    running = false;
    el.run.disabled = false;
  }
}

function render(result) {
  renderDiagnostics(result.compile.diagnostics ?? [], result.compile);
  renderBytecode(el.panels.bytecode, result);
  renderTrace(el.panels.trace, result);
  const { message, kind } = statusFromResult(result);
  setStatus(message, kind);

  // Send the user where the news is: a failed compile makes Diagnostics the
  // only pane that explains anything, so switching there beats leaving them
  // looking at an empty trace.
  if (result.compile.exitCode !== 0) {
    selectTab("diagnostics");
  } else if (activeTab() === "diagnostics" && (result.compile.diagnostics ?? []).length === 0) {
    selectTab("trace");
  }
}

// renderDiagnostics layers the editor-specific bits (inline gutter markers,
// the tab badge) on top of the shared, editor-independent list rendering.
function renderDiagnostics(diagnostics, compile) {
  currentDiagnostics = diagnostics.map((d) => {
    const line = view.state.doc.line(Math.min(Math.max(d.line, 1), view.state.doc.lines));
    return {
      from: line.from,
      to: line.to,
      severity: d.level === "error" ? "error" : "warning",
      message: d.message,
    };
  });
  view.dispatch(setDiagnostics(view.state, currentDiagnostics));

  const badgeCount = diagnostics.length;
  el.badge.hidden = badgeCount === 0;
  el.badge.textContent = String(badgeCount);
  el.badge.classList.toggle(
    "badge-error",
    diagnostics.some((d) => d.level === "error"),
  );

  renderDiagnosticsList(el.panels.diagnostics, diagnostics, compile, { onJumpToLine: gotoLine });
}

function gotoLine(lineNumber) {
  const clamped = Math.min(Math.max(lineNumber, 1), view.state.doc.lines);
  const line = view.state.doc.line(clamped);
  view.dispatch({
    selection: { anchor: line.from },
    effects: EditorView.scrollIntoView(line.from, { y: "center" }),
  });
  view.focus();
}

function showRequestError(message) {
  for (const panel of Object.values(el.panels)) panel.innerHTML = "";
  el.panels.trace.append(note(message, "error"));
  selectTab("trace");
}

/* ------------------------------------------------------------------ */
/* Samples, sharing, tabs                                              */
/* ------------------------------------------------------------------ */

async function loadSamples() {
  try {
    const response = await fetch("/api/samples");
    if (!response.ok) throw new Error(`HTTP ${response.status}`);
    samples = await response.json();
  } catch (err) {
    console.warn("could not load samples:", err);
    el.sampleSelect.disabled = true;
    return;
  }

  for (const sample of samples) {
    const option = document.createElement("option");
    option.value = sample.id;
    option.textContent = sample.name;
    el.sampleSelect.append(option);
  }
}

el.sampleSelect.addEventListener("change", () => {
  const sample = samples.find((s) => s.id === el.sampleSelect.value);
  if (!sample) {
    el.sampleDescription.hidden = true;
    return;
  }
  setSource(sample.source);
  el.sampleDescription.textContent = sample.description;
  el.sampleDescription.hidden = !sample.description;

  // Drop any stale permalink; the URL now claims a program that isn't loaded.
  window.history.replaceState(null, "", window.location.pathname);
  setStatus(`Loaded “${sample.name}”. Press ⌘↵ to run.`, "");
});

el.share.addEventListener("click", async () => {
  const fragment = await encodeSource(getSource());
  const url = window.location.origin + window.location.pathname + fragment;
  window.history.replaceState(null, "", fragment);

  try {
    await navigator.clipboard.writeText(url);
    setStatus("Link copied to the clipboard.", "ok");
  } catch {
    // Clipboard access needs a secure context; over plain HTTP it throws. The
    // URL bar now holds the link either way, so say so instead of failing.
    setStatus("Link is in the address bar — copy it from there.", "warn");
  }
});

el.run.addEventListener("click", runProgram);

function activeTab() {
  return playgroundRoot.querySelector('.tab[aria-selected="true"]')?.dataset.tab ?? "trace";
}

function selectTab(name) {
  for (const tab of playgroundRoot.querySelectorAll(".tab")) {
    const selected = tab.dataset.tab === name;
    tab.setAttribute("aria-selected", String(selected));
    el.panels[tab.dataset.tab].hidden = !selected;
  }
}

for (const tab of playgroundRoot.querySelectorAll(".tab")) {
  tab.addEventListener("click", () => selectTab(tab.dataset.tab));
}

function setStatus(message, kind) {
  el.status.textContent = message;
  el.status.className = `statusbar${kind ? ` statusbar-${kind}` : ""}`;
}


/* ------------------------------------------------------------------ */
/* Boot                                                                */
/* ------------------------------------------------------------------ */

let playgroundBooted = false;

async function bootPlayground() {
  if (playgroundBooted) return;
  playgroundBooted = true;

  await loadSamples();

  const shared = await decodeFragment();
  if (shared) {
    setSource(shared);
    setStatus("Loaded a shared program. Press ⌘↵ to run.", "");
  }

  el.panels.trace.append(note("Press Run, or ⌘↵, to compile and execute.", "muted"));
}

initRouter({
  playground: { mount: bootPlayground },
  dashboard: { mount: (container) => import("./views/dashboard.js").then((m) => m.mount(container)) },
  project: { mount: (container, route) => import("./views/project.js").then((m) => m.mount(container, route)) },
});
