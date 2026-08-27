import { StreamLanguage, HighlightStyle, syntaxHighlighting } from "@codemirror/language";
import { tags as t } from "@lezer/highlight";

// Token classes below are taken from flowcode's compiler (src/compiler.c), not
// guessed from the samples, so the editor agrees with what actually parses.
//
// One thing worth stating plainly because it shapes the whole mode: FlowCode
// has no comment syntax. Both `# ...` and `// ...` compile to
// `warning: unrecognized line`, and compilation still exits 0. So there is no
// comment token here, and the diagnostics panel — not the highlighter — is what
// tells a user their line was ignored.

// is_keyword_line(): declarative keywords that shape the workflow but emit no
// instruction.
const STRUCTURE = new Set([
  "workflow",
  "step",
  "trigger",
  "use",
  "parallel",
  "cron",
  "schedule",
  "continue",
  "break",
  "else",
  "default",
  "stop",
  "end",
]);

// Directives that do produce an opcode, plus the words that bind their clauses.
const DIRECTIVES = new Set([
  "emit",
  "await",
  "store",
  "transform",
  "loop",
  "match",
  "on_error",
  "webhook",
  "in",
  "set",
]);

// is_plugin_call(): a dotted identifier such as `http.post` or `email.send`.
const PLUGIN_CALL = /^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z0-9_]+)+/;
// is_param_line(): keys may be dotted paths or header names like If-Modified-Since.
const PARAM_KEY = /^[A-Za-z0-9][A-Za-z0-9_.\-]*(?=\s*=)/;
const IDENTIFIER = /^[A-Za-z_][A-Za-z0-9_]*/;

export const flowcodeLanguage = StreamLanguage.define({
  name: "flowcode",

  startState() {
    // `inString` survives across tokens so `{{...}}` inside a string literal
    // can be highlighted as its own thing without losing the string context.
    return { inString: false };
  },

  token(stream, state) {
    if (state.inString) {
      return readStringBody(stream, state);
    }

    if (stream.eatSpace()) return null;

    if (stream.match('"')) {
      state.inString = true;
      return "string";
    }

    // Route arms: `all_available ->`
    if (stream.match("->")) return "operator";

    if (stream.match(/^[0-9]+(\.[0-9]+)?\b/)) return "number";

    // Check the parameter key before the plugin-call pattern: a dotted key on
    // the left of an `=` is a field path, not a call.
    if (stream.match(PARAM_KEY)) return "propertyName";

    if (stream.match(PLUGIN_CALL)) return "function";

    if (stream.match(IDENTIFIER)) {
      const word = stream.current();
      if (STRUCTURE.has(word)) return "keyword";
      if (DIRECTIVES.has(word)) return "definitionKeyword";
      return "variableName";
    }

    // Inline data literals: `body = { sku = item.sku }`.
    if (stream.match(/^[{}[\](),=:]/)) return "punctuation";

    stream.next();
    return null;
  },

  languageData: {
    // No `commentTokens`: toggle-comment would otherwise insert syntax the
    // compiler rejects.
    indentOnInput: /^\s*end$/,
    closeBrackets: { brackets: ["(", "[", "{", '"'] },
  },
});

// readStringBody consumes a string literal, breaking out `{{ ... }}` templates
// so interpolation is visible at a glance — it is the language's only form of
// dynamic value, and in practice it's what a reader scans a workflow for.
function readStringBody(stream, state) {
  if (stream.match("{{")) {
    stream.eatWhile((ch) => ch !== "}" && ch !== '"');
    stream.match("}}");
    return "special";
  }

  while (!stream.eol()) {
    if (stream.peek() === '"') {
      stream.next();
      state.inString = false;
      return "string";
    }
    if (stream.match("{{", false)) return "string";
    stream.next();
  }

  // An unterminated literal ends at the newline rather than swallowing the rest
  // of the file, which keeps a half-typed line from recolouring everything.
  state.inString = false;
  return "string";
}

// Two palettes rather than one that "works" in both: the light theme needs
// darker, more saturated hues to stay legible on white, and the dark theme
// needs lighter ones. Both were picked to keep keyword/directive/plugin
// distinguishable without relying on hue alone.
export const flowcodeHighlightLight = HighlightStyle.define([
  { tag: t.keyword, color: "#7c3aed", fontWeight: "600" },
  { tag: t.definitionKeyword, color: "#0369a1", fontWeight: "600" },
  { tag: t.function(t.variableName), color: "#b45309" },
  { tag: t.propertyName, color: "#0f766e" },
  { tag: t.string, color: "#15803d" },
  { tag: t.special(t.string), color: "#c2410c", fontWeight: "600" },
  { tag: t.number, color: "#b45309" },
  { tag: t.operator, color: "#7c3aed", fontWeight: "600" },
  { tag: t.punctuation, color: "#64748b" },
  { tag: t.variableName, color: "#1e293b" },
]);

export const flowcodeHighlightDark = HighlightStyle.define([
  { tag: t.keyword, color: "#c4b5fd", fontWeight: "600" },
  { tag: t.definitionKeyword, color: "#7dd3fc", fontWeight: "600" },
  { tag: t.function(t.variableName), color: "#fcd34d" },
  { tag: t.propertyName, color: "#5eead4" },
  { tag: t.string, color: "#86efac" },
  { tag: t.special(t.string), color: "#fdba74", fontWeight: "600" },
  { tag: t.number, color: "#fcd34d" },
  { tag: t.operator, color: "#c4b5fd", fontWeight: "600" },
  { tag: t.punctuation, color: "#94a3b8" },
  { tag: t.variableName, color: "#e2e8f0" },
]);

export function flowcodeHighlighting(dark) {
  return syntaxHighlighting(dark ? flowcodeHighlightDark : flowcodeHighlightLight);
}
