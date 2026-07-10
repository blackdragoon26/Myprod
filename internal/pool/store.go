package pool

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Store struct {
	dir string
}

func NewStore(dir string) Store {
	return Store{dir: dir}
}

func (s Store) Init() (bool, error) {
	if _, err := os.Stat(s.dir); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}

	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(filepath.Join(s.dir, "config.yaml"), []byte(defaultConfig), 0o644); err != nil {
		return false, err
	}
	if err := os.WriteFile(filepath.Join(s.dir, "state.yaml"), []byte(defaultState), 0o600); err != nil {
		return false, err
	}
	return true, nil
}

func (s Store) Load() (Config, State, error) {
	cfgBytes, err := os.ReadFile(filepath.Join(s.dir, "config.yaml"))
	if err != nil {
		return Config{}, State{}, fmt.Errorf("read config: %w; run poolctl init first", err)
	}
	stateBytes, err := os.ReadFile(filepath.Join(s.dir, "state.yaml"))
	if err != nil {
		return Config{}, State{}, fmt.Errorf("read state: %w; run poolctl init first", err)
	}

	cfg, err := parseConfig(string(cfgBytes))
	if err != nil {
		return Config{}, State{}, err
	}
	state := parseState(string(stateBytes))
	return cfg, state, nil
}

func (s Store) SaveState(state State) error {
	state.ensure()
	return os.WriteFile(filepath.Join(s.dir, "state.yaml"), []byte(formatState(state)), 0o600)
}

func parseConfig(raw string) (Config, error) {
	cfg := Config{}
	var section string
	var currentNode *Node
	var currentApp *App

	flushNode := func() {
		if currentNode != nil {
			cfg.Nodes = append(cfg.Nodes, *currentNode)
			currentNode = nil
		}
	}
	flushApp := func() {
		if currentApp != nil {
			cfg.Apps = append(cfg.Apps, *currentApp)
			currentApp = nil
		}
	}

	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasSuffix(trimmed, ":") && !strings.HasPrefix(trimmed, "- ") {
			switch strings.TrimSuffix(trimmed, ":") {
			case "nodes":
				flushApp()
				section = "nodes"
			case "apps":
				flushNode()
				section = "apps"
			default:
				if section == "" {
					key := strings.TrimSuffix(trimmed, ":")
					if key != "guard" && key != "placement" {
						return Config{}, fmt.Errorf("unsupported top-level section %q", key)
					}
				}
			}
			continue
		}

		if strings.HasPrefix(trimmed, "name:") && section == "" {
			cfg.Name = value(trimmed)
			continue
		}

		if strings.HasPrefix(trimmed, "- name:") {
			if section == "nodes" {
				flushNode()
				currentNode = &Node{Name: strings.TrimSpace(strings.TrimPrefix(trimmed, "- name:"))}
			} else if section == "apps" {
				flushApp()
				currentApp = &App{Name: strings.TrimSpace(strings.TrimPrefix(trimmed, "- name:"))}
			}
			continue
		}

		if section == "nodes" && currentNode != nil {
			applyNodeField(currentNode, trimmed)
		}
		if section == "apps" && currentApp != nil {
			applyAppField(currentApp, trimmed)
		}
	}

	flushNode()
	flushApp()
	if cfg.Name == "" {
		cfg.Name = "personal-compute-pool"
	}
	return cfg, nil
}

func applyNodeField(node *Node, line string) {
	switch {
	case strings.HasPrefix(line, "role:"):
		node.Role = value(line)
	case strings.HasPrefix(line, "provider:"):
		node.Provider = value(line)
	case strings.HasPrefix(line, "cost_mode:"):
		node.CostMode = value(line)
	case strings.HasPrefix(line, "placement:"):
		node.Placement = value(line)
	case strings.HasPrefix(line, "public_ip:"):
		node.PublicIP = value(line)
	case strings.HasPrefix(line, "ssh_user:"):
		node.SSHUser = value(line)
	case strings.HasPrefix(line, "ssh_key:"):
		node.SSHKey = value(line)
	case strings.HasPrefix(line, "overlay_ip:"):
		node.OverlayIP = value(line)
	case strings.HasPrefix(line, "enabled:"):
		node.Guard.Enabled = value(line) == "true"
	case strings.HasPrefix(line, "max_disk_percent:"):
		node.Guard.MaxDiskPercent = atoi(value(line))
	case strings.HasPrefix(line, "max_memory_percent:"):
		node.Guard.MaxMemoryPercent = atoi(value(line))
	case strings.HasPrefix(line, "max_load1:"):
		node.Guard.MaxLoad1, _ = strconv.ParseFloat(value(line), 64)
	}
}

func applyAppField(app *App, line string) {
	switch {
	case strings.HasPrefix(line, "image:"):
		app.Image = value(line)
	case strings.HasPrefix(line, "domain:"):
		app.Domain = value(line)
	case strings.HasPrefix(line, "port:"):
		app.Port = atoi(value(line))
	case strings.HasPrefix(line, "prefer_node:"):
		app.PreferNode = value(line)
	case strings.HasPrefix(line, "allow_workers:"):
		app.AllowWorkers = value(line) == "true"
	}
}

func parseState(raw string) State {
	state := State{Nodes: map[string]NodeState{}, Apps: map[string]AppState{}}
	var section string
	var current string
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if trimmed == "nodes:" || trimmed == "apps:" {
			section = strings.TrimSuffix(trimmed, ":")
			current = ""
			continue
		}
		if strings.HasPrefix(trimmed, "- name:") {
			current = strings.TrimSpace(strings.TrimPrefix(trimmed, "- name:"))
			if section == "nodes" {
				state.Nodes[current] = NodeState{}
			}
			if section == "apps" {
				state.Apps[current] = AppState{}
			}
			continue
		}
		if current == "" {
			continue
		}
		if section == "nodes" {
			node := state.Nodes[current]
			if strings.HasPrefix(trimmed, "frozen:") {
				node.Frozen = value(trimmed) == "true"
			}
			if strings.HasPrefix(trimmed, "draining:") {
				node.Draining = value(trimmed) == "true"
			}
			state.Nodes[current] = node
		}
		if section == "apps" {
			app := state.Apps[current]
			if strings.HasPrefix(trimmed, "node:") {
				app.Node = value(trimmed)
			}
			if strings.HasPrefix(trimmed, "status:") {
				app.Status = value(trimmed)
			}
			state.Apps[current] = app
		}
	}
	return state
}

func formatState(state State) string {
	var b strings.Builder
	b.WriteString("nodes:\n")
	for name, node := range state.Nodes {
		b.WriteString(fmt.Sprintf("  - name: %s\n", name))
		b.WriteString(fmt.Sprintf("    frozen: %t\n", node.Frozen))
		b.WriteString(fmt.Sprintf("    draining: %t\n", node.Draining))
	}
	b.WriteString("apps:\n")
	for name, app := range state.Apps {
		b.WriteString(fmt.Sprintf("  - name: %s\n", name))
		b.WriteString(fmt.Sprintf("    node: %s\n", app.Node))
		b.WriteString(fmt.Sprintf("    status: %s\n", app.Status))
	}
	return b.String()
}

func value(line string) string {
	parts := strings.SplitN(line, ":", 2)
	if len(parts) != 2 {
		return ""
	}
	return strings.Trim(strings.TrimSpace(parts[1]), `"`)
}

func atoi(raw string) int {
	n, _ := strconv.Atoi(raw)
	return n
}

const defaultConfig = `name: personal-compute-pool

nodes:
	- name: oracle-main
    role: control-plane
    provider: oracle
    cost_mode: free_tier_primary
    placement: preferred
    public_ip: 140.245.5.201
    ssh_user: ubuntu
    ssh_key: ~/.ssh/keys/openclaw-oracle.key
    overlay_ip: 10.44.0.1
    guard:
      enabled: true
      max_disk_percent: 80
      max_memory_percent: 85
      max_load1: 3.5

apps:
  - name: sample-api
    image: traefik/whoami:v1.11.0
    domain: api.sankalpjha.dev
    port: 80
    placement:
      prefer_node: oracle-main
      allow_workers: true
`

const defaultState = `nodes:
  - name: oracle-main
    frozen: false
    draining: false
apps: []
`
