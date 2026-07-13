package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/blackdragoon26/Myprod/internal/agent"
	"github.com/blackdragoon26/Myprod/internal/pool"
	"github.com/blackdragoon26/Myprod/internal/web"
)

const usage = `poolctl is a tiny personal compute-pool CLI.

Usage:
  poolctl init
  poolctl render
  poolctl bootstrap-control-plane --dry-run
  poolctl bootstrap-control-plane --apply
  poolctl doctor
  poolctl control-plane status
  poolctl node list
  poolctl node freeze <node>
  poolctl node unfreeze <node>
  poolctl node drain <node>
  poolctl node join <node>
  poolctl app render <app>
  poolctl app deploy <app>
  poolctl app status <app>
  poolctl guard check
  poolctl web
  poolctl agent

Experimental commands are intentionally local-only in this scaffold.
`

func Run(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Print(usage)
		return nil
	}

	store := pool.NewStore(".poolctl")

	switch args[0] {
	case "init":
		return initPool(store)
	case "render":
		return render(store)
	case "bootstrap-control-plane":
		return bootstrapControlPlane(store, args[1:])
	case "control-plane":
		return controlPlane(store, args[1:])
	case "doctor":
		return doctor(store)
	case "node":
		return node(store, args[1:])
	case "app":
		return app(store, args[1:])
	case "guard":
		return guard(store, args[1:])
	case "web":
		return web.Serve(".poolctl", args[1:])
	case "agent":
		return agent.Serve(args[1:])
	default:
		return fmt.Errorf("unknown command %q\n\n%s", args[0], usage)
	}
}

func controlPlane(store pool.Store, args []string) error {
	if len(args) != 1 || args[0] != "status" {
		return errors.New("usage: poolctl control-plane status")
	}
	cfg, _, err := store.Load()
	if err != nil {
		return err
	}
	node, ok := pool.ControlPlaneNode(cfg)
	if !ok {
		return errors.New("config has no control-plane node")
	}
	return pool.CheckControlPlaneStatus(node)
}

func render(store pool.Store) error {
	cfg, _, err := store.Load()
	if err != nil {
		return err
	}
	files, err := pool.RenderControlPlane(cfg)
	if err != nil {
		return err
	}
	const outDir = "work/rendered"
	if err := pool.WriteRendered(outDir, files); err != nil {
		return err
	}
	bundledBinary, err := pool.CopyLocalBinaryIntoBundle(outDir)
	if err != nil {
		return err
	}
	fmt.Printf("rendered %d control-plane files into %s\n", len(files), outDir)
	for _, file := range files {
		fmt.Printf("- %s/%s\n", outDir, file.Path)
	}
	if bundledBinary {
		fmt.Printf("- %s/poolctl\n", outDir)
	}
	return nil
}

func bootstrapControlPlane(store pool.Store, args []string) error {
	if len(args) != 1 || (args[0] != "--dry-run" && args[0] != "--apply") {
		return errors.New("usage: poolctl bootstrap-control-plane --dry-run|--apply")
	}
	cfg, _, err := store.Load()
	if err != nil {
		return err
	}
	node, ok := pool.ControlPlaneNode(cfg)
	if !ok {
		return errors.New("config has no control-plane node")
	}
	if err := render(store); err != nil {
		return err
	}
	if args[0] == "--apply" {
		fmt.Println()
		fmt.Printf("applying control-plane bootstrap to %s@%s\n", node.SSHUser, node.PublicIP)
		return pool.ApplyControlPlaneBundle(node, "work/rendered", "~/poolctl-rendered")
	}
	fmt.Println()
	fmt.Println("dry-run only: no SSH connection was made and no server was changed")
	fmt.Printf("target: %s@%s\n", node.SSHUser, node.PublicIP)
	fmt.Printf("ssh key: %s\n", node.SSHKey)
	fmt.Println()
	fmt.Println("manual review/run command:")
	fmt.Printf("  scp -i %s -r work/rendered %s@%s:~/poolctl-rendered\n", node.SSHKey, node.SSHUser, node.PublicIP)
	fmt.Printf("  ssh -i %s %s@%s 'cd ~/poolctl-rendered && ./bootstrap-control-plane.sh'\n", node.SSHKey, node.SSHUser, node.PublicIP)
	return nil
}

func initPool(store pool.Store) error {
	created, err := store.Init()
	if err != nil {
		return err
	}
	if created {
		fmt.Println("created .poolctl/config.yaml")
		fmt.Println("created .poolctl/state.yaml")
		return nil
	}
	fmt.Println(".poolctl already exists")
	return nil
}

func doctor(store pool.Store) error {
	cfg, state, err := store.Load()
	if err != nil {
		return err
	}

	fmt.Printf("pool: %s\n", cfg.Name)
	fmt.Printf("nodes: %d\n", len(cfg.Nodes))
	fmt.Printf("apps: %d\n", len(cfg.Apps))
	fmt.Printf("frozen nodes: %d\n", state.FrozenCount())
	fmt.Println("status: local config readable")
	return nil
}

func node(store pool.Store, args []string) error {
	if len(args) == 0 {
		return errors.New("missing node subcommand")
	}

	switch args[0] {
	case "list":
		cfg, state, err := store.Load()
		if err != nil {
			return err
		}
		return pool.PrintNodes(cfg, state)
	case "freeze", "unfreeze", "drain":
		if len(args) != 2 {
			return fmt.Errorf("usage: poolctl node %s <node>", args[0])
		}
		return updateNode(store, args[0], args[1])
	case "join":
		if len(args) != 2 {
			return errors.New("usage: poolctl node join <node>")
		}
		cfg, _, err := store.Load()
		if err != nil {
			return err
		}
		if err := pool.JoinWorker(cfg, args[1], "work/rendered/workers/"+args[1]); err != nil {
			return err
		}
		return store.SetNodeJoined(args[1], true)
	default:
		return fmt.Errorf("unknown node subcommand %q", args[0])
	}
}

func updateNode(store pool.Store, action, name string) error {
	cfg, state, err := store.Load()
	if err != nil {
		return err
	}
	if !cfg.HasNode(name) {
		return fmt.Errorf("unknown node %q", name)
	}

	switch action {
	case "freeze":
		state.SetFrozen(name, true)
		fmt.Printf("node %s frozen for new placements\n", name)
	case "unfreeze":
		state.SetFrozen(name, false)
		fmt.Printf("node %s unfrozen\n", name)
	case "drain":
		state.SetDraining(name, true)
		fmt.Printf("node %s marked draining\n", name)
	}

	return store.SaveState(state)
}

func app(store pool.Store, args []string) error {
	if len(args) == 0 {
		return errors.New("missing app subcommand")
	}

	switch args[0] {
	case "status":
		if len(args) != 2 {
			return errors.New("usage: poolctl app status <app>")
		}
		cfg, state, err := store.Load()
		if err != nil {
			return err
		}
		return pool.PrintAppStatus(cfg, state, args[1])
	case "render":
		if len(args) != 2 {
			return errors.New("usage: poolctl app render <app>")
		}
		cfg, _, err := store.Load()
		if err != nil {
			return err
		}
		file, err := pool.RenderAppJob(cfg, args[1])
		if err != nil {
			return err
		}
		const outDir = "work/rendered"
		if err := pool.WriteRendered(outDir, []pool.RenderedFile{file}); err != nil {
			return err
		}
		fmt.Printf("rendered %s/%s\n", outDir, file.Path)
		return nil
	case "deploy":
		if len(args) != 2 {
			return errors.New("usage: poolctl app deploy <app>")
		}
		return deployApp(store, args[1])
	default:
		return fmt.Errorf("unknown app subcommand %q", args[0])
	}
}

func deployApp(store pool.Store, appName string) error {
	cfg, state, err := store.Load()
	if err != nil {
		return err
	}
	node, ok := pool.ControlPlaneNode(cfg)
	if !ok {
		return errors.New("config has no control-plane node")
	}
	app, ok := cfg.FindApp(appName)
	if !ok {
		return fmt.Errorf("unknown app %q", appName)
	}
	file, err := pool.RenderAppJob(cfg, appName)
	if err != nil {
		return err
	}
	const outDir = "work/rendered"
	if err := pool.WriteRendered(outDir, []pool.RenderedFile{file}); err != nil {
		return err
	}
	localPath := outDir + "/" + file.Path
	fmt.Printf("rendered %s\n", localPath)
	if err := pool.DeployAppJob(node, app, localPath, "$HOME/poolctl-jobs"); err != nil {
		return err
	}
	placement := app.PreferNode
	if placement == "" {
		placement = node.Name
	}
	state.SetApp(app.Name, placement, "deployed")
	return store.SaveState(state)
}

func guard(store pool.Store, args []string) error {
	if len(args) != 1 || args[0] != "check" {
		return errors.New("usage: poolctl guard check")
	}

	cfg, state, err := store.Load()
	if err != nil {
		return err
	}
	result := pool.CheckGuard(cfg, state)
	if len(result.Warnings) == 0 {
		fmt.Println("guard: ok")
		return store.SaveState(state)
	}
	fmt.Println("guard warnings:")
	for _, warning := range result.Warnings {
		fmt.Printf("- %s\n", warning)
	}
	if len(result.FrozenNodes) > 0 {
		fmt.Printf("frozen: %s\n", strings.Join(result.FrozenNodes, ", "))
	}
	return store.SaveState(state)
}
