/*
 * fcplay — the FlowCode playground's runtime driver.
 *
 * This is flowcode's own src/cli.c with two deliberate differences:
 *
 *   1. The log level is turned down to DEBUG before the VM runs. The stock CLI
 *      leaves it at the FC_LOG_WARN default, which suppresses "vm starting",
 *      every builtin plugin invocation, and "vm completed successfully" — so a
 *      successful `flowcode run` prints nothing at all. A playground whose Run
 *      button produces an empty pane is a broken playground; this trace is the
 *      output users actually came to see.
 *
 *   2. Resource limits are installed on this process before anything is loaded.
 *      The VM has no fuel counter — exec_loop/exec_route in vm.c assign
 *      frame->ip = target unconditionally, so a backward jump spins forever.
 *      This is the process that can spin, so the limits belong here rather
 *      in a separate wrapper binary. The server still applies its own wall-clock
 *      timeout on top; RLIMIT_CPU only counts CPU time, so a process blocked on
 *      something would slip past it.
 *
 *   3. After the run, every key a `store set` wrote is dumped as a
 *      `store set key = "..." value = "..."` trace line. STORE is an opcode,
 *      not a plugin call, so the VM logs nothing when it executes — without
 *      this dump the server's KV log (which parses those lines out of the
 *      trace; see server/internal/store/kv.go) would always be empty.
 *
 * Everything else — builtin registration, the default token seed — is kept
 * verbatim from cli.c. Dropping either breaks the samples that `store` before
 * they `emit`.
 *
 * Links against flowcode's sources using only its public headers.
 */

#include "flowcode.h"
#include "fc_builtins.h"
#include "fc_error.h"
#include "fc_log.h"

#include <stdio.h>
#include <string.h>

#ifndef _WIN32
#include <sys/resource.h>
#include <sys/time.h>
#endif

/* Kept in sync with the server's own timeout, which is the outer bound. */
#define FCPLAY_CPU_SECONDS   2
#define FCPLAY_ADDRESS_SPACE (256u * 1024u * 1024u)
#define FCPLAY_MAX_FILE_SIZE (1u * 1024u * 1024u)
#define FCPLAY_MAX_OPEN_FILES 64

static const char default_token_payload[] = FC_BUILTIN_DEFAULT_TOKEN;

static void usage(void) {
    fprintf(stderr, "usage: fcplay <file.fcb>\n");
}

#ifndef _WIN32
static void limit(int resource, rlim_t value) {
    struct rlimit rl;
    if (getrlimit(resource, &rl) != 0) return;
    /* Never raise a limit the environment already set lower than we want. */
    if (rl.rlim_cur != RLIM_INFINITY && rl.rlim_cur <= value) return;
    rl.rlim_cur = value;
    if (rl.rlim_max != RLIM_INFINITY && rl.rlim_max < value) rl.rlim_cur = rl.rlim_max;
    (void)setrlimit(resource, &rl);
}
#endif

/* Matches the VM's own key buffer (src/vm.c defines the same constant
 * privately): a longer STORE argument is skipped rather than truncated,
 * exactly like exec_store's own overflow behaviour. */
#define FCPLAY_KEY_MAX 256

/* Keeps one dumped value to a sane share of the 64 KB stderr cap the server
 * enforces; longer values are truncated with an ellipsis. */
#define FCPLAY_VALUE_DISPLAY 512

/* Synthetic STOREs the compiler emits for on_error strategies (src/compiler.c)
 * are control flow, not user data — keep them out of the dump. */
static int is_synthetic_store_key(const char *key) {
    return strncmp(key, "on_error:", 9) == 0 || strncmp(key, "__on_error.", 11) == 0;
}

/* Emit one line in the exact shape server/internal/store/kv.go parses. The
 * value is sanitized to survive being a single quoted token on one line:
 * a '"' would end the server's capture early, a control character would
 * break the line. */
static void log_store_set(const char *key, const void *value, uint32_t size) {
    const unsigned char *in = (const unsigned char *)value;
    char vbuf[FCPLAY_VALUE_DISPLAY + 8];
    uint32_t limit = size > FCPLAY_VALUE_DISPLAY ? FCPLAY_VALUE_DISPLAY : size;
    uint32_t i;
    size_t n = 0;

    for (i = 0; i < limit; ++i) {
        unsigned char ch = in[i];
        if (ch == '"') ch = '\'';
        else if (ch < 0x20 || ch == 0x7f) ch = ' ';
        vbuf[n++] = (char)ch;
    }
    if (size > limit) {
        vbuf[n++] = '.';
        vbuf[n++] = '.';
        vbuf[n++] = '.';
    }
    vbuf[n] = '\0';
    fc_log(FC_LOG_INFO, "store set key = \"%s\" value = \"%s\"", key, vbuf);
}

/*
 * Dump the final value of every key the program's STORE instructions target.
 *
 * The state store exposes lookup but no iteration, so the key list comes from
 * the bytecode itself: each STORE's argument, or "last_token" for the keyless
 * form exec_store falls back to. Semantics this buys:
 *
 *   - a key stored and re-stored (a loop, two branches) prints once, with
 *     its final value — the log records what the run left behind;
 *   - a STORE whose arm never ran is skipped, because its key never entered
 *     the store and fc_state_get reports it missing;
 *   - error stores (`__error.<ip>`, written directly by the VM) are never
 *     listed as instructions, so they stay out too.
 */
static void dump_stores(const fc_program_t *program, fc_state_store_t *state) {
    char seen[64][FCPLAY_KEY_MAX];
    uint32_t seen_count = 0;
    uint32_t i;

    for (i = 0; i < program->instruction_count; ++i) {
        const fc_instruction_t *ins = &program->instructions[i];
        char key[FCPLAY_KEY_MAX];
        const void *value;
        uint32_t size;
        uint32_t j;

        if (ins->opcode != FC_OP_STORE) continue;

        if (ins->arg_length == 0) {
            memcpy(key, "last_token", sizeof("last_token"));
        } else {
            if (ins->arg_length >= sizeof(key)) continue;
            memcpy(key, &program->arg_blob[ins->arg_offset], ins->arg_length);
            key[ins->arg_length] = '\0';
        }
        if (is_synthetic_store_key(key)) continue;

        for (j = 0; j < seen_count; ++j) {
            if (strcmp(seen[j], key) == 0) break;
        }
        if (j < seen_count) continue;
        if (seen_count < (uint32_t)(sizeof(seen) / sizeof(seen[0]))) {
            strcpy(seen[seen_count], key);
            seen_count++;
        }

        if (fc_state_get(state, key, &value, &size) == 0 && value != NULL)
            log_store_set(key, value, size);
    }
}

/*
 * Best-effort: a failed setrlimit is not worth aborting a run over, because the
 * server's timeout and the container's own memory cap both still apply.
 */
static void install_limits(void) {
#ifndef _WIN32
#ifdef RLIMIT_CPU
    limit(RLIMIT_CPU, (rlim_t)FCPLAY_CPU_SECONDS);
#endif
#ifdef RLIMIT_AS
    limit(RLIMIT_AS, (rlim_t)FCPLAY_ADDRESS_SPACE);
#endif
#ifdef RLIMIT_FSIZE
    limit(RLIMIT_FSIZE, (rlim_t)FCPLAY_MAX_FILE_SIZE);
#endif
#ifdef RLIMIT_NOFILE
    limit(RLIMIT_NOFILE, (rlim_t)FCPLAY_MAX_OPEN_FILES);
#endif
#ifdef RLIMIT_NPROC
    /* The VM never forks. Zero additional processes is the honest budget. */
    limit(RLIMIT_NPROC, (rlim_t)0);
#endif
#endif /* !_WIN32 */
}

static int run_fcb(const char *path) {
    fc_program_t program;
    fc_state_store_t *state;
    fc_plugin_registry_t *plugins;
    fc_vm_t *vm;
    int rc;

    if (fc_program_load_file(path, &program) != 0) {
        fprintf(stderr, "error: failed to load bytecode file: %s\n", path);
        return 1;
    }

    state = fc_state_create();
    plugins = fc_plugins_create();
    vm = fc_vm_create(&program, state, plugins);
    if (!state || !plugins || !vm) {
        fprintf(stderr, "error: failed to initialize runtime\n");
        fc_program_free(&program);
        fc_state_destroy(state);
        fc_plugins_destroy(plugins);
        fc_vm_destroy(vm);
        return 1;
    }

    if (fc_plugins_register_builtins(plugins) != 0) {
        fprintf(stderr, "error: failed to register built-in plugins\n");
        fc_vm_destroy(vm);
        fc_plugins_destroy(plugins);
        fc_state_destroy(state);
        fc_program_free(&program);
        return 1;
    }
    /* Seed a token so a workflow that stores before it emits still runs. */
    fc_vm_set_default_token(vm, default_token_payload,
                            (uint32_t)(sizeof(default_token_payload) - 1u));

    rc = fc_vm_run(vm);
    if (rc != 0) {
        const fc_error_t *err = fc_vm_last_error(vm);
        if (err && err->code != FC_ERR_OK) {
            fprintf(stderr, "error: workflow failed at instruction %u: %s",
                    err->instruction_index, fc_error_name(err->code));
            if (err->message[0])
                fprintf(stderr, " - %s", err->message);
            fprintf(stderr, "\n");
        } else {
            fprintf(stderr, "error: workflow execution failed\n");
        }
    }

    /* Whatever the run's fate, the stores that did land are the trace's only
     * record of what the workflow wrote — dump them while the state store and
     * the program are still alive. */
    dump_stores(&program, state);

    fc_vm_destroy(vm);
    fc_plugins_destroy(plugins);
    fc_state_destroy(state);
    fc_program_free(&program);
    return rc == 0 ? 0 : 1;
}

int main(int argc, char **argv) {
    if (argc != 2) {
        usage();
        return 1;
    }

    install_limits();

    /* The whole point: make the run observable. */
    fc_log_set_level(FC_LOG_DEBUG);

    /* stderr is unbuffered by default, but the trace is the product here and a
     * SIGKILL on timeout must not cost us the lines already written. */
    setvbuf(stderr, NULL, _IONBF, 0);

    return run_fcb(argv[1]);
}
