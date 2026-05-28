//go:build !voice

package main

func bootVoice(cfg *Config, st *StateStore) {}
func shutdownVoice()                        {}
