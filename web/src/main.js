import { EditorView, keymap, lineNumbers, highlightActiveLine } from "@codemirror/view";
import { EditorState, Compartment } from "@codemirror/state";
import { defaultKeymap, history, historyKeymap, indentWithTab } from "@codemirror/commands";
import { bracketMatching, indentUnit } from "@codemirror/language";
import { linter, lintGutter, setDiagnostics } from "@codemirror/lint";

import { flowcodeLanguage, flowcodeHighlighting } from "./flowcode-lang.js";
import { encodeSource, decodeFragment } from "./share.js";
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
  renderBytecode(result);
  renderTrace(result);
  setStatusFromResult(result);

  // Send the user where the news is: a failed compile makes Diagnostics the
  // only pane that explains anything, so switching there beats leaving them
  // looking at an empty trace.
  if (result.compile.exitCode !== 0) {
    selectTab("diagnostics");
  } else if (activeTab() === "diagnostics" && (result.compile.diagnostics ?? []).length === 0) {
    selectTab("trace");
  }
}

function renderTrace(result) {
  const panel = el.panels.trace;
  panel.innerHTML = "";

  if (result.compile.exitCode !== 0) {
    panel.append(
      note("Compilation failed, so nothing was executed. See the Diagnostics tab.", "warn"),
    );
    return;
  }

  const run = result.run;
  if (!run) {
    panel.append(note("The program was not executed.", "warn"));
    return;
  }

  if (run.timedOut) {
    panel.append(
      note(
        "The program was stopped after exceeding the time limit. FlowCode's VM has no " +
          "instruction budget, so a workflow that jumps backwards runs forever.",
        "error",
      ),
    );
  }

  const trace = run.stderr || run.stdout;
  if (trace) {
    panel.append(traceBlock(trace));
  } else if (!run.timedOut) {
    panel.append(note("The program produced no output.", "muted"));
  }

  if (run.error) {
    panel.append(note(run.error, "error"));
  }
  if (result.truncated) {
    panel.append(note("Output was truncated at 64 KB.", "muted"));
  }
}

// The runtime tags every line `[flowcode:LEVEL]`; splitting that off lets the
// levels be colour-coded and keeps the message column aligned.
const TRACE_LINE = /^\[flowcode:(DEBUG|INFO|WARN|ERROR)\]\s*(.*)$/;

function traceBlock(text) {
  const pre = document.createElement("pre");
  pre.className = "trace";

  for (const raw of text.replace(/\n$/, "").split("\n")) {
    const line = document.createElement("div");
    line.className = "trace-line";

    const match = TRACE_LINE.exec(raw);
    if (match) {
      const [, level, message] = match;
      const tag = document.createElement("span");
      tag.className = `trace-level trace-level-${level.toLowerCase()}`;
      tag.textContent = level;
      line.append(tag, document.createTextNode(message));
    } else {
      line.classList.add("trace-line-plain");
      line.textContent = raw;
    }
    pre.append(line);
  }
  return pre;
}

function renderBytecode(result) {
  const panel = el.panels.bytecode;
  panel.innerHTML = "";

  if (result.bytecodeError) {
    panel.append(note(`Could not decode the bytecode: ${result.bytecodeError}`, "error"));
    return;
  }
  const bc = result.bytecode;
  if (!bc) {
    panel.append(note("No bytecode was produced.", "muted"));
    return;
  }

  const summary = document.createElement("p");
  summary.className = "bytecode-summary";
  summary.textContent =
    `FCB v${bc.version} · ${bc.instructionCount} instruction${bc.instructionCount === 1 ? "" : "s"} · ` +
    `${bc.argBlobSize} B arguments · ${bc.sizeBytes} B total`;
  panel.append(summary);

  const table = document.createElement("table");
  table.className = "bytecode";
  table.innerHTML =
    "<thead><tr><th>#</th><th>Opcode</th><th>Argument</th><th>Offset</th><th>Len</th></tr></thead>";

  const tbody = document.createElement("tbody");
  for (const ins of bc.instructions) {
    const tr = document.createElement("tr");
    tr.id = `instr-${ins.index}`;

    tr.append(
      cell(String(ins.index), "num"),
      cell(ins.opcode, `opcode opcode-${ins.opcode.toLowerCase()}`),
    );

    // ROUTE/LOOP arguments are jump targets; make them navigable rather than
    // leaving the reader to scroll and count.
    const argCell = document.createElement("td");
    argCell.className = "arg";
    if (ins.target !== undefined) {
      const link = document.createElement("a");
      link.href = `#instr-${ins.target}`;
      link.textContent = ins.arg;
      link.className = "jump";
      link.addEventListener("click", (e) => {
        e.preventDefault();
        highlightInstruction(ins.target);
      });
      argCell.append(link);
    } else {
      argCell.textContent = ins.arg;
    }
    tr.append(argCell, cell(String(ins.argOffset), "num"), cell(String(ins.argLength), "num"));
    tbody.append(tr);
  }

  table.append(tbody);
  panel.append(table);
}

function highlightInstruction(index) {
  const row = document.getElementById(`instr-${index}`);
  if (!row) return;
  row.scrollIntoView({ behavior: "smooth", block: "center" });
  row.classList.remove("flash");
  // Force a reflow so the animation restarts when the same row is targeted twice.
  void row.offsetWidth;
  row.classList.add("flash");
}

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

  const panel = el.panels.diagnostics;
  panel.innerHTML = "";

  if (badgeCount === 0) {
    panel.append(
      note(
        compile.exitCode === 0
          ? "No diagnostics — the program compiled cleanly."
          : "The compiler reported no positioned diagnostics.",
        compile.exitCode === 0 ? "ok" : "warn",
      ),
    );
  } else {
    const list = document.createElement("ul");
    list.className = "diagnostics";
    for (const d of diagnostics) {
      const item = document.createElement("li");
      item.className = `diagnostic diagnostic-${d.level}`;

      const jump = document.createElement("button");
      jump.type = "button";
      jump.className = "diagnostic-line";
      jump.textContent = `line ${d.line}`;
      jump.addEventListener("click", () => gotoLine(d.line));

      const level = document.createElement("span");
      level.className = "diagnostic-level";
      level.textContent = d.level;

      const message = document.createElement("span");
      message.className = "diagnostic-message";
      message.textContent = d.message;

      item.append(level, jump, message);
      list.append(item);
    }
    panel.append(list);
  }

  // Unpositioned compiler output — "compilation completed with errors", file
  // I/O failures — has nowhere else to go, and hiding it would leave some
  // failures looking unexplained.
  const extra = unpositionedLines(compile.stderr ?? "");
  if (extra) {
    const pre = document.createElement("pre");
    pre.className = "raw-stderr";
    pre.textContent = extra;
    panel.append(pre);
  }
}

function unpositionedLines(stderr) {
  return stderr
    .split("\n")
    .filter((line) => line.trim() && !/^(error|warning): line \d+:/.test(line))
    .join("\n");
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

function setStatusFromResult(result) {
  const { compile, run } = result;

  if (compile.timedOut) {
    setStatus("The compiler timed out.", "error");
    return;
  }
  if (compile.exitCode !== 0) {
    setStatus(`Compilation failed (exit ${compile.exitCode}).`, "error");
    return;
  }
  if (!run) {
    setStatus("Compiled, but the program was not executed.", "warn");
    return;
  }
  if (run.timedOut) {
    setStatus("Stopped: the program exceeded the time limit.", "error");
    return;
  }

  const warnings = (compile.diagnostics ?? []).length;
  const suffix = warnings > 0 ? ` · ${warnings} warning${warnings === 1 ? "" : "s"}` : "";
  const timing = `compiled in ${compile.durationMs} ms, ran in ${run.durationMs} ms`;

  if (run.exitCode !== 0) {
    setStatus(`The workflow failed (exit ${run.exitCode}) · ${timing}${suffix}`, "error");
  } else {
    setStatus(`Finished · ${timing}${suffix}`, warnings > 0 ? "warn" : "ok");
  }
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
  return document.querySelector('.tab[aria-selected="true"]')?.dataset.tab ?? "trace";
}

function selectTab(name) {
  for (const tab of document.querySelectorAll(".tab")) {
    const selected = tab.dataset.tab === name;
    tab.setAttribute("aria-selected", String(selected));
    el.panels[tab.dataset.tab].hidden = !selected;
  }
}

for (const tab of document.querySelectorAll(".tab")) {
  tab.addEventListener("click", () => selectTab(tab.dataset.tab));
}

function setStatus(message, kind) {
  el.status.textContent = message;
  el.status.className = `statusbar${kind ? ` statusbar-${kind}` : ""}`;
}

function cell(text, className) {
  const td = document.createElement("td");
  td.className = className;
  td.textContent = text;
  return td;
}

function note(text, kind) {
  const p = document.createElement("p");
  p.className = `note note-${kind}`;
  p.textContent = text;
  return p;
}

/* ------------------------------------------------------------------ */
/* Boot                                                                */
/* ------------------------------------------------------------------ */

(async function init() {
  await loadSamples();

  const shared = await decodeFragment();
  if (shared) {
    setSource(shared);
    setStatus("Loaded a shared program. Press ⌘↵ to run.", "");
  }

  el.panels.trace.append(note("Press Run, or ⌘↵, to compile and execute.", "muted"));
})();
