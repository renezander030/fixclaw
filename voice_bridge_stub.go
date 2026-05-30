//go:build !voice

package main

import statestore "github.com/renezander030/draftcat/internal/state"

// Lean build (no -tags voice): voice is compiled out entirely. These no-ops
// satisfy the calls in main without importing internal/voicebridge, so the
// voice server, Dograh client, and their deps stay out of the binary.

func bootVoice(cfg *Config, st *statestore.StateStore) {}

func shutdownVoice() {}

func tryVoiceAction(action, pipelineName string, vars map[string]string, data map[string]interface{}) (bool, bool, error) {
	return false, false, nil
}
