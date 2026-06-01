package acp

import "sync"

type RuntimeVersionInfo struct {
	Commit    string `json:"commit,omitempty"`
	Version   string `json:"version,omitempty"`
	BuildDate string `json:"buildDate,omitempty"`
}

var runtimeVersionState = struct {
	sync.RWMutex
	info RuntimeVersionInfo
}{}

func SetRuntimeVersionInfo(info RuntimeVersionInfo) {
	runtimeVersionState.Lock()
	defer runtimeVersionState.Unlock()
	runtimeVersionState.info = info
}

func CurrentRuntimeVersionInfo() RuntimeVersionInfo {
	runtimeVersionState.RLock()
	defer runtimeVersionState.RUnlock()
	return runtimeVersionState.info
}
