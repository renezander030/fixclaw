//go:build voice

package main

import (
	"context"
	"log"
	"time"

	"github.com/renezander030/draftyard/voice"
)

var voiceServer *voice.Server

func bootVoice(cfg *Config, st *StateStore) {
	if st == nil {
		return
	}
	var vcfg voice.Config
	if err := cfg.Voice.Decode(&vcfg); err != nil {
		log.Printf("[voice] config decode failed (skipping voice): %v", err)
		return
	}
	if !vcfg.Enabled {
		return
	}
	srv, err := voice.NewServer(vcfg, st.DB())
	if err != nil {
		log.Printf("[voice] init failed: %v", err)
		return
	}
	if srv == nil {
		return
	}
	voiceServer = srv
	go func() {
		if err := srv.Start(); err != nil && err.Error() != "http: Server closed" {
			log.Printf("[voice] server stopped: %v", err)
		}
	}()
}

func shutdownVoice() {
	if voiceServer == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := voiceServer.Shutdown(ctx); err != nil {
		log.Printf("[voice] shutdown error: %v", err)
	}
}
