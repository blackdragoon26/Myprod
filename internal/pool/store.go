package pool

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

func (s Store) AddNode(node Node) error {
	cfg, state, err := s.Load()
	if err != nil {
		return err
	}
	if err := validateNewNode(cfg, node); err != nil {
		return err
	}
	if node.Role == "" {
		node.Role = "worker"
	}
	if node.Provider == "" {
		node.Provider = "digitalocean"
	}
	if node.CostMode == "" {
		node.CostMode = "credit_temporary"
	}
	if node.Placement == "" {
		node.Placement = "burst"
	}
	if node.Guard.MaxDiskPercent == 0 {
		node.Guard.MaxDiskPercent = 80
	}
	if node.Guard.MaxMemoryPercent == 0 {
		node.Guard.MaxMemoryPercent = 85
	}
	if node.Guard.MaxLoad1 == 0 {
		node.Guard.MaxLoad1 = 3.5
	}

	cfg.Nodes = append(cfg.Nodes, node)
	state.ensure()
	if _, ok := state.Nodes[node.Name]; !ok {
		state.Nodes[node.Name] = NodeState{}
	}
	if err := os.WriteFile(filepath.Join(s.dir, "config.yaml"), []byte(formatConfig(cfg)), 0o644); err != nil {
		return err
	}
	return s.SaveState(state)
}

func (s Store) SetNodeJoined(name string, joined bool) error {
	cfg, state, err := s.Load()
	if err != nil {
		return err
	}
	if !cfg.HasNode(name) {
		return fmt.Errorf("unknown node %q", name)
	}
	state.SetJoined(name, joined)
	return s.SaveState(state)
}

func validateNewNode(cfg Config, node Node) error {
	if node.Name == "" {
		return errors.New("missing node name")
	}
	if !safeID(node.Name) {
		return errors.New("node name may contain only letters, numbers, dash, and underscore")
	}
	if cfg.HasNode(node.Name) {
		return fmt.Errorf("node %q already exists", node.Name)
	}
	if node.PublicIP == "" {
		return errors.New("missing public IP")
	}
	if node.SSHUser == "" {
		return errors.New("missing SSH user")
	}
	if node.SSHKey == "" {
		return errors.New("missing SSH key path")
	}
	if node.OverlayIP == "" {
		return errors.New("missing overlay IP")
	}
	for _, existing := range cfg.Nodes {
		if existing.OverlayIP == node.OverlayIP {
			return fmt.Errorf("overlay IP %q is already used by %s", node.OverlayIP, existing.Name)
		}
	}
	return nil
}

func safeID(raw string) bool {
	for _, r := range raw {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return raw != ""
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
			if strings.HasPrefix(trimmed, "joined:") {
				node.Joined = value(trimmed) == "true"
			}
			if strings.HasPrefix(trimmed, "reserved_for:") {
				node.ReservedFor = value(trimmed)
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
	for _, name := range sortedNodeStateNames(state) {
		node := state.Nodes[name]
		b.WriteString(fmt.Sprintf("  - name: %s\n", name))
		b.WriteString(fmt.Sprintf("    frozen: %t\n", node.Frozen))
		b.WriteString(fmt.Sprintf("    draining: %t\n", node.Draining))
		b.WriteString(fmt.Sprintf("    joined: %t\n", node.Joined))
		b.WriteString(fmt.Sprintf("    reserved_for: %s\n", node.ReservedFor))
	}
	b.WriteString("apps:\n")
	for _, name := range sortedAppStateNames(state) {
		app := state.Apps[name]
		b.WriteString(fmt.Sprintf("  - name: %s\n", name))
		b.WriteString(fmt.Sprintf("    node: %s\n", app.Node))
		b.WriteString(fmt.Sprintf("    status: %s\n", app.Status))
	}
	return b.String()
}

func sortedNodeStateNames(state State) []string {
	names := make([]string, 0, len(state.Nodes))
	for name := range state.Nodes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedAppStateNames(state State) []string {
	names := make([]string, 0, len(state.Apps))
	for name := range state.Apps {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func formatConfig(cfg Config) string {
	var b strings.Builder
	if cfg.Name == "" {
		cfg.Name = "personal-compute-pool"
	}
	b.WriteString(fmt.Sprintf("name: %s\n\n", cfg.Name))
	b.WriteString("nodes:\n")
	for _, node := range cfg.Nodes {
		b.WriteString(fmt.Sprintf("  - name: %s\n", node.Name))
		b.WriteString(fmt.Sprintf("    role: %s\n", node.Role))
		b.WriteString(fmt.Sprintf("    provider: %s\n", node.Provider))
		b.WriteString(fmt.Sprintf("    cost_mode: %s\n", node.CostMode))
		b.WriteString(fmt.Sprintf("    placement: %s\n", node.Placement))
		b.WriteString(fmt.Sprintf("    public_ip: %s\n", node.PublicIP))
		b.WriteString(fmt.Sprintf("    ssh_user: %s\n", node.SSHUser))
		b.WriteString(fmt.Sprintf("    ssh_key: %s\n", node.SSHKey))
		b.WriteString(fmt.Sprintf("    overlay_ip: %s\n", node.OverlayIP))
		b.WriteString("    guard:\n")
		b.WriteString(fmt.Sprintf("      enabled: %t\n", node.Guard.Enabled))
		b.WriteString(fmt.Sprintf("      max_disk_percent: %d\n", node.Guard.MaxDiskPercent))
		b.WriteString(fmt.Sprintf("      max_memory_percent: %d\n", node.Guard.MaxMemoryPercent))
		b.WriteString(fmt.Sprintf("      max_load1: %.1f\n", node.Guard.MaxLoad1))
	}
	b.WriteString("\napps:\n")
	for _, app := range cfg.Apps {
		b.WriteString(fmt.Sprintf("  - name: %s\n", app.Name))
		b.WriteString(fmt.Sprintf("    image: %s\n", app.Image))
		b.WriteString(fmt.Sprintf("    domain: %s\n", app.Domain))
		b.WriteString(fmt.Sprintf("    port: %d\n", app.Port))
		b.WriteString("    placement:\n")
		b.WriteString(fmt.Sprintf("      prefer_node: %s\n", app.PreferNode))
		b.WriteString(fmt.Sprintf("      allow_workers: %t\n", app.AllowWorkers))
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
