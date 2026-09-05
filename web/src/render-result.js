// Shared rendering for an engine.Result-shaped JSON body — the Trace,
// Bytecode, and Diagnostics panes. Used by both the anonymous playground and
// the project Run panel, so a run's output looks and behaves identically no
// matter which surface produced it.

export function note(text, kind) {
  const p = document.createElement("p");
  p.className = `note note-${kind}`;
  p.textContent = text;
  return p;
}

export function cell(text, className) {
  const td = document.createElement("td");
  td.className = className;
  td.textContent = text;
  return td;
}

// The runtime tags every line `[flowcode:LEVEL]`; splitting that off lets the
// levels be colour-coded and keeps the message column aligned.
const TRACE_LINE = /^\[flowcode:(DEBUG|INFO|WARN|ERROR)\]\s*(.*)$/;

export function traceBlock(text) {
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

export function renderTrace(panel, result) {
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

export function highlightInstruction(index) {
  const row = document.getElementById(`instr-${index}`);
  if (!row) return;
  row.scrollIntoView({ behavior: "smooth", block: "center" });
  row.classList.remove("flash");
  // Force a reflow so the animation restarts when the same row is targeted twice.
  void row.offsetWidth;
  row.classList.add("flash");
}

export function renderBytecode(panel, result) {
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

export function unpositionedLines(stderr) {
  return (stderr ?? "")
    .split("\n")
    .filter((line) => line.trim() && !/^(error|warning): line \d+:/.test(line))
    .join("\n");
}

// renderDiagnosticsList renders the Diagnostics pane's list and raw-stderr
// spillover. It does not touch an editor — callers that have one (the
// playground) layer inline gutter markers on top separately, since a plain
// project-file Run panel has no editor to mark up.
export function renderDiagnosticsList(panel, diagnostics, compile, { onJumpToLine } = {}) {
  panel.innerHTML = "";

  if (diagnostics.length === 0) {
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

      const level = document.createElement("span");
      level.className = "diagnostic-level";
      level.textContent = d.level;

      let lineEl;
      if (onJumpToLine) {
        lineEl = document.createElement("button");
        lineEl.type = "button";
        lineEl.className = "diagnostic-line";
        lineEl.addEventListener("click", () => onJumpToLine(d.line));
      } else {
        lineEl = document.createElement("span");
        lineEl.className = "diagnostic-line";
      }
      lineEl.textContent = `line ${d.line}`;

      const message = document.createElement("span");
      message.className = "diagnostic-message";
      message.textContent = d.message;

      item.append(level, lineEl, message);
      list.append(item);
    }
    panel.append(list);
  }

  const extra = unpositionedLines(compile.stderr);
  if (extra) {
    const pre = document.createElement("pre");
    pre.className = "raw-stderr";
    pre.textContent = extra;
    panel.append(pre);
  }
}

// statusFromResult is a pure function so both the playground's statusbar and
// the project view's own can render the same summary without duplicating the
// logic that produces it.
export function statusFromResult(result) {
  const { compile, run } = result;

  if (compile.timedOut) {
    return { message: "The compiler timed out.", kind: "error" };
  }
  if (compile.exitCode !== 0) {
    return { message: `Compilation failed (exit ${compile.exitCode}).`, kind: "error" };
  }
  if (!run) {
    return { message: "Compiled, but the program was not executed.", kind: "warn" };
  }
  if (run.timedOut) {
    return { message: "Stopped: the program exceeded the time limit.", kind: "error" };
  }

  const warnings = (compile.diagnostics ?? []).length;
  const suffix = warnings > 0 ? ` · ${warnings} warning${warnings === 1 ? "" : "s"}` : "";
  const timing = `compiled in ${compile.durationMs} ms, ran in ${run.durationMs} ms`;

  if (run.exitCode !== 0) {
    return { message: `The workflow failed (exit ${run.exitCode}) · ${timing}${suffix}`, kind: "error" };
  }
  return { message: `Finished · ${timing}${suffix}`, kind: warnings > 0 ? "warn" : "ok" };
}

// renderResult renders all three panels at once and picks the tab that has
// the news, mirroring the playground's original behavior.
export function renderResult(panels, result, { selectTab, activeTab, onJumpToLine } = {}) {
  renderDiagnosticsList(panels.diagnostics, result.compile.diagnostics ?? [], result.compile, { onJumpToLine });
  renderBytecode(panels.bytecode, result);
  renderTrace(panels.trace, result);

  if (!selectTab) return;
  if (result.compile.exitCode !== 0) {
    selectTab("diagnostics");
  } else if (activeTab?.() === "diagnostics" && (result.compile.diagnostics ?? []).length === 0) {
    selectTab("trace");
  }
}
