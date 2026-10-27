package store

import (
	"regexp"
)

// KVEntry is one row of a project's best-effort, write-side key/value log
// (ws_kv_store). Populated by parsing the `store set` dump lines out of the
// run trace after the fact — one entry per key, holding the run's final
// value for it. It is a debug/audit log a human can inspect from the
// dashboard, not a working key-value API a running script can read from.
type KVEntry struct {
	ProjectID         int64  `json:"projectId"`
	Key               string `json:"key"`
	Value             string `json:"value"`
	UpdatedAt         string `json:"updatedAt"`
	SourceExecutionID *int64 `json:"sourceExecutionId,omitempty"`
}

// storeSetRe matches the trace line fcplay emits after a run for every key a
// `store set` wrote (runner/fcplay.c, dump_stores): exactly
//
//	[flowcode:INFO] store set key = "demo.greeting" value = "hello"
//
// The VM itself logs STORE opcodes not at all — only plugin calls are traced —
// so fcplay owns both sides of this contract and the entrypoint's build check
// pins the format by grepping a known sample's dump. Values arrive pre-sanitized
// (quotes become apostrophes, control characters spaces) and truncated at 512
// bytes so they always fit the single quoted token this regex captures.
var storeSetRe = regexp.MustCompile(`(?i)store\s+set\s+key\s*=\s*"([^"]*)"\s+value\s*=\s*"([^"]*)"`)

// ExtractStoreSets pulls the `store set` lines out of a run trace, keyed by
// store key with the run's final value for each. It returns no error on zero
// matches — an empty result is a normal, expected outcome for a workflow that
// never calls `store set`.
func ExtractStoreSets(trace string) map[string]string {
	writes := map[string]string{}
	for _, m := range storeSetRe.FindAllStringSubmatch(trace, -1) {
		writes[m[1]] = m[2]
	}
	return writes
}
