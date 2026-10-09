# URL to Markdown

Show a form, take a website address from the user, fetch that page, strip the
HTML tags and page furniture, and store what is left as Markdown.

This is the smallest realistic "human gives input → workflow does the fetch and
the cleanup → result gets persisted" shape, so it is a good second sample after
`samples/hello-world/`.

## Modules

| Module    | Purpose                                             |
|-----------|-----------------------------------------------------|
| `form`    | Render the URL input form and await the submission  |
| `http`    | GET the page at the submitted address               |
| `ai`      | Convert the stripped HTML into Markdown             |
| `storage` | Archive the Markdown document                       |

## Flow overview

1. **Ask** — `form.render` puts a one-field form (`url`) in front of the user.
2. **Await** — the workflow parks at `await form.submit` until the form comes
   back. Nothing downstream runs until then.
3. **Validate** — `transform validate.url` normalizes the address and marks it
   valid or invalid.
4. **Branch** — `match validatedUrl.status` emits `invalid_url` on a bad
   address; on a good one it proceeds to the fetch.
5. **Fetch** — `http.get` retrieves the page, with `on_error retry = 3` and a
   30-second timeout, because the far end of a user-supplied URL is the least
   reliable part of this workflow.
6. **Strip** — `transform html.strip` drops the tags and the fluff: `script`,
   `style`, `nav`, `header`, `footer`, `aside`, `form`, `iframe`, `noscript`.
7. **Convert** — `ai.generate` turns the remaining content into Markdown, then
   `transform markdown.tidy` collapses the leftover blank lines and stray
   entities.
8. **Store** — the Markdown goes to the `page-archive` bucket via
   `storage.upload`, and a `{source_url, body}` document goes into the state
   store under `markdown.<host>`.
9. **Emit** — `markdown_saved` for anything downstream.

## Where the Markdown ends up

Two places, deliberately:

- `storage.upload` — the durable copy, `page-archive/<host>.md`.
- `store set` — the in-run copy under `markdown.<host>`, which an embedder
  reads with `fc_state_get(state, "markdown.<host>", &value, &size)`.

The state store is in-memory and lives only for the duration of the run, so
after a CLI run there is no file to inspect. The durable write is the
`storage.upload` step, and what that actually does is up to the plugin you
load for `storage.*`.

## Running

From a source checkout run `make` first; from a release archive the binaries
are already next to `samples/`.

```bash
./fcc samples/url-to-markdown/url-to-markdown.fc url-to-markdown.fcb
./flowcode run url-to-markdown.fcb
```

A successful run exits 0.

## What actually happens on a CLI run

Nothing touches the network. The `flowcode` CLI registers no-I/O stubs for
`http.*`, `form.*`, `ai.*`, and `storage.*`: each one logs the call and passes
the token through, so the workflow runs end to end with no credentials and no
side effects. That is what makes this sample runnable as-is.

To make it do the real work, load plugin libraries that export those names —
they replace the stubs:

```c
fc_plugins_register_builtins(registry);      /* stubs first */
fc_plugins_load(registry, "./libhttp.so");   /* real http.* wins */
fc_plugins_load(registry, "./libform.so");
```

The `transform` steps (`validate.url`, `html.strip`, `markdown.tidy`,
`combine`) are named for the host application to implement; the compiler
records the name and the VM passes the token through.

Run with `--strict` to turn the stubs off and have unresolved calls fail with
`FC_ERR_PLUGIN_NOT_FOUND` — useful for checking that you have wired up every
integration the workflow names.

## Capabilities demonstrated

- Human input as the workflow's entry point (`form.render` + `await`)
- Blocking on an external event without blocking the scheduler
- Input validation with a `match` branch for the bad case
- Retry and timeout policy on the one step that talks to the open internet
- Content transformation chained across several `transform` steps
- Writing to both durable storage and the state store
- Event emission for downstream consumers
