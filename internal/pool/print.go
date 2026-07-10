package pool

import (
	"fmt"
)

func PrintNodes(cfg Config, state State) error {
	fmt.Println("NAME\tROLE\tPROVIDER\tPUBLIC\tOVERLAY\tSTATE")
	for _, node := range cfg.Nodes {
		nodeState := state.Nodes[node.Name]
		label := "ready"
		if nodeState.Draining {
			label = "draining"
		} else if nodeState.Frozen {
			label = "frozen"
		}
		fmt.Printf("%s\t%s\t%s\t%s\t%s\t%s\n", node.Name, node.Role, node.Provider, node.PublicIP, node.OverlayIP, label)
	}
	return nil
}

func PrintAppStatus(cfg Config, state State, name string) error {
	app, ok := cfg.FindApp(name)
	if !ok {
		return fmt.Errorf("unknown app %q", name)
	}
	appState := state.Apps[name]
	if appState.Status == "" {
		appState.Status = "not-deployed"
	}
	if appState.Node == "" {
		appState.Node = "<none>"
	}

	fmt.Printf("app: %s\n", app.Name)
	fmt.Printf("image: %s\n", app.Image)
	fmt.Printf("domain: %s\n", app.Domain)
	fmt.Printf("port: %d\n", app.Port)
	fmt.Printf("preferred node: %s\n", app.PreferNode)
	fmt.Printf("current node: %s\n", appState.Node)
	fmt.Printf("status: %s\n", appState.Status)
	return nil
}
