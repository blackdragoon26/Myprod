package pool

type Config struct {
	Name  string
	Nodes []Node
	Apps  []App
}

type Node struct {
	Name      string
	Role      string
	Provider  string
	CostMode  string
	Placement string
	PublicIP  string
	SSHUser   string
	SSHKey    string
	OverlayIP string
	Guard     Guard
}

type Guard struct {
	Enabled          bool
	MaxDiskPercent   int
	MaxMemoryPercent int
	MaxLoad1         float64
}

type App struct {
	Name         string
	Image        string
	Domain       string
	Port         int
	PreferNode   string
	AllowWorkers bool
	CPU          int
	MemoryMB     int
	HealthPath   string
}

type State struct {
	Nodes map[string]NodeState
	Apps  map[string]AppState
}

type NodeState struct {
	Frozen      bool
	Draining    bool
	Joined      bool
	ReservedFor string
}

type AppState struct {
	Node   string
	Status string
}

func (c Config) HasNode(name string) bool {
	for _, node := range c.Nodes {
		if node.Name == name {
			return true
		}
	}
	return false
}

func (c Config) FindNode(name string) (Node, bool) {
	for _, node := range c.Nodes {
		if node.Name == name {
			return node, true
		}
	}
	return Node{}, false
}

func (c Config) FindApp(name string) (App, bool) {
	for _, app := range c.Apps {
		if app.Name == name {
			return app, true
		}
	}
	return App{}, false
}

func (s State) FrozenCount() int {
	total := 0
	for _, node := range s.Nodes {
		if node.Frozen {
			total++
		}
	}
	return total
}

func (s *State) SetFrozen(name string, frozen bool) {
	s.ensure()
	node := s.Nodes[name]
	node.Frozen = frozen
	if frozen {
		node.Draining = false
	}
	s.Nodes[name] = node
}

func (s *State) SetDraining(name string, draining bool) {
	s.ensure()
	node := s.Nodes[name]
	node.Draining = draining
	if draining {
		node.Frozen = true
	}
	s.Nodes[name] = node
}

func (s *State) SetJoined(name string, joined bool) {
	s.ensure()
	node := s.Nodes[name]
	node.Joined = joined
	s.Nodes[name] = node
}

func (s *State) SetReserved(name, project string) {
	s.ensure()
	node := s.Nodes[name]
	node.ReservedFor = project
	if project != "" {
		node.Frozen = true
		node.Draining = false
	}
	s.Nodes[name] = node
}

func (s *State) SetApp(name, node, status string) {
	s.ensure()
	s.Apps[name] = AppState{Node: node, Status: status}
}

func (s *State) ensure() {
	if s.Nodes == nil {
		s.Nodes = make(map[string]NodeState)
	}
	if s.Apps == nil {
		s.Apps = make(map[string]AppState)
	}
}
