# Hello World

The smallest useful Flowcode workflow: emit a value, then put it in the state
store. No integrations, no branching, no plugins — start here to check that your
binaries work before reading any of the other samples.

## The workflow

```
workflow: HelloWorld

step greeting:
    emit
        value = "hello, world"
end

step saved:
    store set
        key = "greeting"
        value = greeting
end
```

Two instructions come out of this:

| Instruction | Opcode         | Meaning                                        |
|-------------|----------------|------------------------------------------------|
| 0           | `FC_OP_EMIT`   | Produce a token carrying `hello, world`        |
| 1           | `FC_OP_STORE`  | Write that token to the state store under `greeting` |

A `step` is a label, not an instruction — it names the work for readers and for
`match`/`loop` targets. The token emitted by one step is what the next step
operates on, which is why `saved` can store `greeting` without naming it again
at runtime.

## Running

Compile the workflow, then run the bytecode. Both binaries ship in the release
archive (`flowcode-<version>-<platform>.tar.gz`); from a source checkout run
`make` first and prefix them with `./`.

```bash
fcc hello.fc hello.fcb
flowcode run hello.fcb
```

A successful run exits 0 and prints nothing but its lifecycle logs:

```
[flowcode:INFO] vm starting, 2 instructions
[flowcode:INFO] vm completed successfully
```

The state store is in-memory and lives only for the duration of the run, so
there is no file to inspect afterwards — embedders read the stored value with
`fc_state_get(state, "greeting", &value, &size)`.

## Next

- `samples/customer-onboarding/` — the full tour: validation, human review,
  branching, loops, parallel work, and plugin integrations.
- The other directories under `samples/` for individual patterns.
