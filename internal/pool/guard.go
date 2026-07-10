package pool

import (
	"fmt"
	"runtime"
)

type GuardResult struct {
	Warnings    []string
	FrozenNodes []string
}

func CheckGuard(cfg Config, state State) GuardResult {
	state.ensure()
	result := GuardResult{}

	for _, node := range cfg.Nodes {
		if !node.Guard.Enabled {
			continue
		}
		if node.Role != "control-plane" {
			continue
		}

		// This local scaffold cannot inspect a remote Oracle VM yet. It still
		// exercises the state transition used by the real systemd guard.
		if node.Guard.MaxMemoryPercent > 0 && runtime.NumGoroutine() > 100000 {
			state.SetFrozen(node.Name, true)
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s crossed local guard threshold", node.Name))
			result.FrozenNodes = append(result.FrozenNodes, node.Name)
		}
	}

	return result
}
