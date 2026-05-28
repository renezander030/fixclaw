//go:build !voice

package main

func tryVoiceAction(action, pipelineName string, vars map[string]string, data map[string]interface{}) (handled bool, skipPipeline bool, err error) {
	return false, false, nil
}
