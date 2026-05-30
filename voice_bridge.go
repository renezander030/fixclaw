//go:build voice

package main

import (
	statestore "github.com/renezander030/draftcat/internal/state"
	"github.com/renezander030/draftcat/internal/voicebridge"
)

// vbridge holds the running voice plugin (nil when voice is disabled in config).
// All heavy lifting lives in internal/voicebridge; this file is the thin,
// build-tag-gated seam between the engine (package main) and that package, so
// the lean binary (built without -tags voice, see voice_bridge_stub.go) never
// links the voice/dograh code.
var vbridge *voicebridge.Bridge

func bootVoice(cfg *Config, st *statestore.StateStore) {
	vbridge = voicebridge.Boot(cfg.Voice, st, ghl)
}

func shutdownVoice() { vbridge.Shutdown() }

func tryVoiceAction(action, pipelineName string, vars map[string]string, data map[string]interface{}) (bool, bool, error) {
	return vbridge.TryAction(action, pipelineName, vars, data)
}
