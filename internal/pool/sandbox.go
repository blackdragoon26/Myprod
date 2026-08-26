package pool

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Sandbox partitions are throwaway Ubuntu ARM64 containers that an LLM or
// automation agent can drive without touching pool infrastructure. They are
// stored outside config.yaml on purpose: a sandbox is never a managed app, is
// never routed by Traefik, and must never be able to change the configuration
// that real applications are rendered from.

const (
	// SandboxJobPrefix namespaces every sandbox Nomad job. Stop/purge paths
	// refuse any job ID without this prefix, so the sandbox surface cannot be
	// used to remove a managed application job.
	SandboxJobPrefix = "poolctl-sbx-"
	// SandboxTaskName is the only task inside a rendered sandbox job.
	SandboxTaskName = "sandbox"
	// SandboxDockerNetwork is the dedicated, inter-container-isolated Docker
	// network created by the host isolation script.
	SandboxDockerNetwork = "poolctl-sandbox"
	// SandboxNetworkCIDR is the fixed subnet the isolation firewall matches on.
	SandboxNetworkCIDR = "172.31.240.0/24"

	sandboxesFilename = "sandboxes.json"

	// SandboxProfileStrict keeps a read-only root filesystem and drops every
	// Linux capability. Nothing can be installed; code can be run and read.
	SandboxProfileStrict = "strict"
	// SandboxProfileWorkspace keeps the container filesystem writable and
	// restores only the capability subset apt/dpkg needs. It never grants
	// SYS_ADMIN, NET_ADMIN, NET_RAW, SYS_PTRACE, SYS_MODULE, or MKNOD.
	SandboxProfileWorkspace = "workspace"

	// SandboxNetworkNone gives the container loopback only.
	SandboxNetworkNone = "none"
	// SandboxNetworkEgress attaches the container to the isolated sandbox
	// bridge, which the host firewall restricts to public destinations.
	SandboxNetworkEgress = "egress"

	SandboxStatusStarting  = "starting"
	SandboxStatusRunning   = "running"
	SandboxStatusExpired   = "expired"
	SandboxStatusDestroyed = "destroyed"
	SandboxStatusFailed    = "failed"

	// Bounds. A sandbox may never reserve more than a small slice of a worker,
	// and may never outlive the operator's attention span by default.
	SandboxMinCPU       = 100
	SandboxMaxCPU       = 4000
	SandboxMinMemoryMB  = 128
	SandboxMaxMemoryMB  = 8192
	SandboxMinDiskMB    = 128
	SandboxMaxDiskMB    = 8192
	SandboxMinTTL       = 60
	SandboxMaxTTL       = 14400
	SandboxDefaultTTL   = 1800
	SandboxDefaultPIDs  = 512
	SandboxMaxPIDs      = 4096
	sandboxHistoryLimit = 64
)

var fallbackSandboxMu sync.Mutex

// DefaultSandboxImages is the image allowlist. Sandboxes exist to provide an
// Ubuntu ARM64 environment, not to become a general way to run arbitrary
// third-party images inside the pool.
var DefaultSandboxImages = []string{
	"ubuntu:",
	"ubuntu@sha256:",
	"docker.io/library/ubuntu:",
	"docker.io/library/ubuntu@sha256:",
	"public.ecr.aws/ubuntu/ubuntu:",
	"public.ecr.aws/ubuntu/ubuntu@sha256:",
}

// DefaultSandboxImage is used when a request omits the image.
const DefaultSandboxImage = "ubuntu:24.04"

// SandboxHost records a node an operator has explicitly enrolled for sandbox
// work, together with the ceiling sandboxes may consume on it. A node that is
// not enrolled can never receive a sandbox.
type SandboxHost struct {
	Node          string    `json:"node"`
	EnrolledAt    time.Time `json:"enrolledAt"`
	MaxSandboxes  int       `json:"maxSandboxes"`
	MaxCPU        int       `json:"maxCpu"`
	MaxMemoryMB   int       `json:"maxMemoryMb"`
	EgressAllowed bool      `json:"egressAllowed"`
}

// Sandbox is the operator-visible record. It deliberately carries no secret:
// the session token exists only as a digest in the private record type.
type Sandbox struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Image      string    `json:"image"`
	Node       string    `json:"node"`
	Profile    string    `json:"profile"`
	Network    string    `json:"network"`
	CPU        int       `json:"cpu"`
	MemoryMB   int       `json:"memoryMb"`
	DiskMB     int       `json:"diskMb"`
	PIDsLimit  int       `json:"pidsLimit"`
	TTLSeconds int       `json:"ttlSeconds"`
	CreatedAt  time.Time `json:"createdAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
	Status     string    `json:"status"`
	JobID      string    `json:"jobId"`
	Note       string    `json:"note,omitempty"`
}

type sandboxRecord struct {
	Sandbox
	TokenDigest string `json:"tokenDigest"`
}

type sandboxFile struct {
	Version   int             `json:"version"`
	Hosts     []SandboxHost   `json:"hosts"`
	Sandboxes []sandboxRecord `json:"sandboxes"`
}

// Active reports whether a sandbox still owns capacity on its node.
func (s Sandbox) Active() bool {
	return s.Status == SandboxStatusStarting || s.Status == SandboxStatusRunning
}

// Expired reports whether the sandbox has outlived its TTL.
func (s Sandbox) Expired(now time.Time) bool {
	return s.Active() && !s.ExpiresAt.IsZero() && now.After(s.ExpiresAt)
}

// SandboxJobID derives the Nomad job name for a sandbox ID.
func SandboxJobID(id string) string {
	return SandboxJobPrefix + id
}

// IsSandboxJobID reports whether a Nomad job name belongs to the sandbox
// surface. Callers that stop or purge jobs must gate on this.
func IsSandboxJobID(jobID string) bool {
	return strings.HasPrefix(jobID, SandboxJobPrefix) && len(jobID) > len(SandboxJobPrefix)
}

// SandboxImageAllowed enforces the Ubuntu base-image allowlist.
func SandboxImageAllowed(image string, allowlist []string) bool {
	if !safeImageReference(image) {
		return false
	}
	if len(allowlist) == 0 {
		allowlist = DefaultSandboxImages
	}
	for _, prefix := range allowlist {
		if strings.HasPrefix(image, prefix) && len(image) > len(prefix) {
			return true
		}
	}
	return false
}

func (s Store) sandboxMutex() *sync.Mutex {
	if s.sandboxMu != nil {
		return s.sandboxMu
	}
	return &fallbackSandboxMu
}

// EnrollSandboxHost marks a node as eligible to run sandboxes. Control-plane
// nodes are refused: a sandbox must never share a machine with Nomad servers,
// Traefik, the agent, or the pool's TLS material.
func (s Store) EnrollSandboxHost(host SandboxHost) (SandboxHost, error) {
	mu := s.sandboxMutex()
	mu.Lock()
	defer mu.Unlock()

	cfg, _, err := s.Load()
	if err != nil {
		return SandboxHost{}, err
	}
	node, ok := cfg.FindNode(host.Node)
	if !ok {
		return SandboxHost{}, fmt.Errorf("unknown node %q", host.Node)
	}
	if node.Role == "control-plane" {
		return SandboxHost{}, errors.New("control-plane nodes cannot host sandboxes")
	}
	if host.MaxSandboxes <= 0 {
		host.MaxSandboxes = 2
	}
	if host.MaxCPU <= 0 {
		host.MaxCPU = 1000
	}
	if host.MaxMemoryMB <= 0 {
		host.MaxMemoryMB = 1024
	}
	if host.MaxSandboxes > 16 {
		return SandboxHost{}, errors.New("a sandbox host may allow at most 16 concurrent sandboxes")
	}
	if host.MaxCPU > SandboxMaxCPU*4 || host.MaxMemoryMB > SandboxMaxMemoryMB*4 {
		return SandboxHost{}, errors.New("sandbox host budget exceeds the supported ceiling")
	}
	host.EnrolledAt = time.Now().UTC()

	file, err := s.loadSandboxFile()
	if err != nil {
		return SandboxHost{}, err
	}
	replaced := false
	for i := range file.Hosts {
		if file.Hosts[i].Node == host.Node {
			file.Hosts[i] = host
			replaced = true
			break
		}
	}
	if !replaced {
		file.Hosts = append(file.Hosts, host)
	}
	if err := s.writeSandboxFile(file); err != nil {
		return SandboxHost{}, err
	}
	return host, nil
}

// RemoveSandboxHost withdraws sandbox eligibility. It refuses while sandboxes
// are still live on that node so capacity accounting cannot be orphaned.
func (s Store) RemoveSandboxHost(node string) error {
	mu := s.sandboxMutex()
	mu.Lock()
	defer mu.Unlock()

	file, err := s.loadSandboxFile()
	if err != nil {
		return err
	}
	for _, record := range file.Sandboxes {
		if record.Node == node && record.Active() {
			return fmt.Errorf("sandbox %s is still active on %s; destroy it first", record.ID, node)
		}
	}
	hosts := file.Hosts[:0]
	found := false
	for _, host := range file.Hosts {
		if host.Node == node {
			found = true
			continue
		}
		hosts = append(hosts, host)
	}
	if !found {
		return fmt.Errorf("node %q is not enrolled as a sandbox host", node)
	}
	file.Hosts = hosts
	return s.writeSandboxFile(file)
}

// ListSandboxHosts returns enrolled hosts sorted by node name.
func (s Store) ListSandboxHosts() ([]SandboxHost, error) {
	mu := s.sandboxMutex()
	mu.Lock()
	defer mu.Unlock()
	file, err := s.loadSandboxFile()
	if err != nil {
		return nil, err
	}
	hosts := append([]SandboxHost(nil), file.Hosts...)
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].Node < hosts[j].Node })
	return hosts, nil
}

// FindSandboxHost returns the enrollment record for a node.
func (s Store) FindSandboxHost(node string) (SandboxHost, bool, error) {
	hosts, err := s.ListSandboxHosts()
	if err != nil {
		return SandboxHost{}, false, err
	}
	for _, host := range hosts {
		if host.Node == node {
			return host, true, nil
		}
	}
	return SandboxHost{}, false, nil
}

// CreateSandbox validates a request against the node enrollment and its
// budget, then persists the sandbox with a freshly minted scoped session
// token. The plaintext token is returned exactly once.
func (s Store) CreateSandbox(request Sandbox) (string, Sandbox, error) {
	mu := s.sandboxMutex()
	mu.Lock()
	defer mu.Unlock()

	cfg, state, err := s.Load()
	if err != nil {
		return "", Sandbox{}, err
	}
	sandbox, err := sandboxWithDefaults(request)
	if err != nil {
		return "", Sandbox{}, err
	}
	node, ok := cfg.FindNode(sandbox.Node)
	if !ok {
		return "", Sandbox{}, fmt.Errorf("unknown node %q", sandbox.Node)
	}
	if node.Role == "control-plane" {
		return "", Sandbox{}, errors.New("control-plane nodes cannot host sandboxes")
	}
	nodeState := state.Nodes[sandbox.Node]
	if nodeState.Frozen || nodeState.Draining || nodeState.ReservedFor != "" {
		return "", Sandbox{}, fmt.Errorf("node %s is unavailable: frozen=%t draining=%t reserved_for=%q",
			sandbox.Node, nodeState.Frozen, nodeState.Draining, nodeState.ReservedFor)
	}

	file, err := s.loadSandboxFile()
	if err != nil {
		return "", Sandbox{}, err
	}
	host, ok := findHost(file.Hosts, sandbox.Node)
	if !ok {
		return "", Sandbox{}, fmt.Errorf("node %q is not enrolled as a sandbox host", sandbox.Node)
	}
	if sandbox.Network == SandboxNetworkEgress && !host.EgressAllowed {
		return "", Sandbox{}, fmt.Errorf("node %q is enrolled without egress; run the sandbox isolation script and re-enroll with egress before requesting network access", sandbox.Node)
	}
	if err := checkSandboxBudget(file.Sandboxes, host, sandbox); err != nil {
		return "", Sandbox{}, err
	}

	id, err := newSandboxID(file.Sandboxes)
	if err != nil {
		return "", Sandbox{}, err
	}
	secret, err := randomDeployTokenPart(32)
	if err != nil {
		return "", Sandbox{}, err
	}
	plaintext := "poolctl_sbx_v1." + id + "." + secret

	now := time.Now().UTC()
	sandbox.ID = id
	sandbox.JobID = SandboxJobID(id)
	sandbox.CreatedAt = now
	sandbox.ExpiresAt = now.Add(time.Duration(sandbox.TTLSeconds) * time.Second)
	sandbox.Status = SandboxStatusStarting

	if _, ok := cfg.FindApp(sandbox.JobID); ok {
		return "", Sandbox{}, fmt.Errorf("sandbox job name %q collides with a managed app", sandbox.JobID)
	}

	file.Sandboxes = append(file.Sandboxes, sandboxRecord{Sandbox: sandbox, TokenDigest: deployTokenDigest(plaintext)})
	file.Sandboxes = pruneSandboxHistory(file.Sandboxes)
	if err := s.writeSandboxFile(file); err != nil {
		return "", Sandbox{}, err
	}
	return plaintext, sandbox, nil
}

// ListSandboxes returns every retained sandbox record, newest first.
func (s Store) ListSandboxes() ([]Sandbox, error) {
	mu := s.sandboxMutex()
	mu.Lock()
	defer mu.Unlock()
	file, err := s.loadSandboxFile()
	if err != nil {
		return nil, err
	}
	sandboxes := make([]Sandbox, 0, len(file.Sandboxes))
	for _, record := range file.Sandboxes {
		sandboxes = append(sandboxes, record.Sandbox)
	}
	sort.Slice(sandboxes, func(i, j int) bool { return sandboxes[i].CreatedAt.After(sandboxes[j].CreatedAt) })
	return sandboxes, nil
}

// FindSandbox returns one sandbox by ID.
func (s Store) FindSandbox(id string) (Sandbox, bool, error) {
	mu := s.sandboxMutex()
	mu.Lock()
	defer mu.Unlock()
	file, err := s.loadSandboxFile()
	if err != nil {
		return Sandbox{}, false, err
	}
	for _, record := range file.Sandboxes {
		if record.ID == id {
			return record.Sandbox, true, nil
		}
	}
	return Sandbox{}, false, nil
}

// AuthorizeSandboxToken reports whether a bearer credential is the session
// token of exactly this sandbox, and whether that sandbox is still live. A
// token for another sandbox never authorizes this one.
func (s Store) AuthorizeSandboxToken(id, plaintext string) (bool, error) {
	mu := s.sandboxMutex()
	mu.Lock()
	defer mu.Unlock()
	if id == "" || plaintext == "" {
		return false, nil
	}
	file, err := s.loadSandboxFile()
	if err != nil {
		return false, err
	}
	digest := deployTokenDigest(plaintext)
	authorized := false
	for _, record := range file.Sandboxes {
		idMatches := subtle.ConstantTimeCompare([]byte(record.ID), []byte(id)) == 1
		digestMatches := subtle.ConstantTimeCompare([]byte(record.TokenDigest), []byte(digest)) == 1
		if idMatches && digestMatches && record.Active() {
			authorized = true
		}
	}
	return authorized, nil
}

// UpdateSandbox records lifecycle transitions. Status is the only mutable
// field besides the note; capacity fields stay as they were budgeted.
func (s Store) UpdateSandbox(id, status, note string) (Sandbox, error) {
	mu := s.sandboxMutex()
	mu.Lock()
	defer mu.Unlock()
	file, err := s.loadSandboxFile()
	if err != nil {
		return Sandbox{}, err
	}
	for i := range file.Sandboxes {
		if file.Sandboxes[i].ID != id {
			continue
		}
		if status != "" {
			if !validSandboxStatus(status) {
				return Sandbox{}, fmt.Errorf("invalid sandbox status %q", status)
			}
			file.Sandboxes[i].Status = status
		}
		file.Sandboxes[i].Note = normalizeSandboxNote(note)
		if err := s.writeSandboxFile(file); err != nil {
			return Sandbox{}, err
		}
		return file.Sandboxes[i].Sandbox, nil
	}
	return Sandbox{}, fmt.Errorf("unknown sandbox %q", id)
}

// ExtendSandbox pushes the expiry out, never past the absolute maximum
// lifetime measured from creation.
func (s Store) ExtendSandbox(id string, extra time.Duration) (Sandbox, error) {
	mu := s.sandboxMutex()
	mu.Lock()
	defer mu.Unlock()
	if extra <= 0 {
		return Sandbox{}, errors.New("extension must be positive")
	}
	file, err := s.loadSandboxFile()
	if err != nil {
		return Sandbox{}, err
	}
	for i := range file.Sandboxes {
		if file.Sandboxes[i].ID != id {
			continue
		}
		if !file.Sandboxes[i].Active() {
			return Sandbox{}, fmt.Errorf("sandbox %s is %s", id, file.Sandboxes[i].Status)
		}
		limit := file.Sandboxes[i].CreatedAt.Add(SandboxMaxTTL * time.Second)
		extended := file.Sandboxes[i].ExpiresAt.Add(extra)
		if extended.After(limit) {
			return Sandbox{}, fmt.Errorf("a sandbox may not live longer than %d seconds after creation", SandboxMaxTTL)
		}
		file.Sandboxes[i].ExpiresAt = extended
		file.Sandboxes[i].TTLSeconds = int(extended.Sub(file.Sandboxes[i].CreatedAt).Seconds())
		if err := s.writeSandboxFile(file); err != nil {
			return Sandbox{}, err
		}
		return file.Sandboxes[i].Sandbox, nil
	}
	return Sandbox{}, fmt.Errorf("unknown sandbox %q", id)
}

// ExpiredSandboxes returns live sandboxes whose TTL has elapsed.
func (s Store) ExpiredSandboxes(now time.Time) ([]Sandbox, error) {
	sandboxes, err := s.ListSandboxes()
	if err != nil {
		return nil, err
	}
	var expired []Sandbox
	for _, sandbox := range sandboxes {
		if sandbox.Expired(now) {
			expired = append(expired, sandbox)
		}
	}
	return expired, nil
}

// ActiveSandboxes returns sandboxes that still hold capacity.
func (s Store) ActiveSandboxes() ([]Sandbox, error) {
	sandboxes, err := s.ListSandboxes()
	if err != nil {
		return nil, err
	}
	var active []Sandbox
	for _, sandbox := range sandboxes {
		if sandbox.Active() {
			active = append(active, sandbox)
		}
	}
	return active, nil
}

// ForgetSandbox drops a sandbox record and its token digest entirely.
func (s Store) ForgetSandbox(id string) error {
	mu := s.sandboxMutex()
	mu.Lock()
	defer mu.Unlock()
	file, err := s.loadSandboxFile()
	if err != nil {
		return err
	}
	filtered := file.Sandboxes[:0]
	found := false
	for _, record := range file.Sandboxes {
		if record.ID == id {
			found = true
			continue
		}
		filtered = append(filtered, record)
	}
	if !found {
		return fmt.Errorf("unknown sandbox %q", id)
	}
	file.Sandboxes = filtered
	return s.writeSandboxFile(file)
}

func sandboxWithDefaults(sandbox Sandbox) (Sandbox, error) {
	sandbox.Name = strings.TrimSpace(sandbox.Name)
	sandbox.Image = strings.TrimSpace(sandbox.Image)
	sandbox.Node = strings.TrimSpace(sandbox.Node)
	sandbox.Profile = strings.TrimSpace(sandbox.Profile)
	sandbox.Network = strings.TrimSpace(sandbox.Network)

	if sandbox.Name == "" {
		sandbox.Name = "llm-sandbox"
	}
	if sandbox.Image == "" {
		sandbox.Image = DefaultSandboxImage
	}
	if sandbox.Profile == "" {
		sandbox.Profile = SandboxProfileStrict
	}
	if sandbox.Network == "" {
		sandbox.Network = SandboxNetworkNone
	}
	if sandbox.CPU == 0 {
		sandbox.CPU = 500
	}
	if sandbox.MemoryMB == 0 {
		sandbox.MemoryMB = 512
	}
	if sandbox.DiskMB == 0 {
		sandbox.DiskMB = 512
	}
	if sandbox.PIDsLimit == 0 {
		sandbox.PIDsLimit = SandboxDefaultPIDs
	}
	if sandbox.TTLSeconds == 0 {
		sandbox.TTLSeconds = SandboxDefaultTTL
	}

	if len(sandbox.Name) > 48 || !safeID(sandbox.Name) {
		return Sandbox{}, errors.New("sandbox name may contain only letters, numbers, dash, and underscore and must be 48 characters or fewer")
	}
	if !SandboxImageAllowed(sandbox.Image, nil) {
		return Sandbox{}, fmt.Errorf("sandbox image must be an allowed Ubuntu base image (%s)", strings.Join(DefaultSandboxImages, ", "))
	}
	if sandbox.Node == "" {
		return Sandbox{}, errors.New("sandbox node is required")
	}
	if !safeID(sandbox.Node) {
		return Sandbox{}, errors.New("sandbox node name is invalid")
	}
	if sandbox.Profile != SandboxProfileStrict && sandbox.Profile != SandboxProfileWorkspace {
		return Sandbox{}, fmt.Errorf("sandbox profile must be %q or %q", SandboxProfileStrict, SandboxProfileWorkspace)
	}
	if sandbox.Network != SandboxNetworkNone && sandbox.Network != SandboxNetworkEgress {
		return Sandbox{}, fmt.Errorf("sandbox network must be %q or %q", SandboxNetworkNone, SandboxNetworkEgress)
	}
	if sandbox.CPU < SandboxMinCPU || sandbox.CPU > SandboxMaxCPU {
		return Sandbox{}, fmt.Errorf("sandbox CPU must be between %d and %d MHz", SandboxMinCPU, SandboxMaxCPU)
	}
	if sandbox.MemoryMB < SandboxMinMemoryMB || sandbox.MemoryMB > SandboxMaxMemoryMB {
		return Sandbox{}, fmt.Errorf("sandbox memory must be between %d and %d MB", SandboxMinMemoryMB, SandboxMaxMemoryMB)
	}
	if sandbox.DiskMB < SandboxMinDiskMB || sandbox.DiskMB > SandboxMaxDiskMB {
		return Sandbox{}, fmt.Errorf("sandbox disk must be between %d and %d MB", SandboxMinDiskMB, SandboxMaxDiskMB)
	}
	if sandbox.PIDsLimit < 32 || sandbox.PIDsLimit > SandboxMaxPIDs {
		return Sandbox{}, fmt.Errorf("sandbox pids limit must be between 32 and %d", SandboxMaxPIDs)
	}
	if sandbox.TTLSeconds < SandboxMinTTL || sandbox.TTLSeconds > SandboxMaxTTL {
		return Sandbox{}, fmt.Errorf("sandbox ttl must be between %d and %d seconds", SandboxMinTTL, SandboxMaxTTL)
	}
	sandbox.Note = normalizeSandboxNote(sandbox.Note)
	return sandbox, nil
}

func checkSandboxBudget(records []sandboxRecord, host SandboxHost, sandbox Sandbox) error {
	count := 0
	cpu := 0
	memory := 0
	for _, record := range records {
		if record.Node != host.Node || !record.Active() {
			continue
		}
		count++
		cpu += record.CPU
		memory += record.MemoryMB
	}
	if count+1 > host.MaxSandboxes {
		return fmt.Errorf("node %s already runs %d/%d sandboxes", host.Node, count, host.MaxSandboxes)
	}
	if cpu+sandbox.CPU > host.MaxCPU {
		return fmt.Errorf("sandbox CPU budget for %s is %d MHz; %d MHz is already reserved", host.Node, host.MaxCPU, cpu)
	}
	if memory+sandbox.MemoryMB > host.MaxMemoryMB {
		return fmt.Errorf("sandbox memory budget for %s is %d MB; %d MB is already reserved", host.Node, host.MaxMemoryMB, memory)
	}
	return nil
}

func findHost(hosts []SandboxHost, node string) (SandboxHost, bool) {
	for _, host := range hosts {
		if host.Node == node {
			return host, true
		}
	}
	return SandboxHost{}, false
}

func newSandboxID(records []sandboxRecord) (string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		raw := make([]byte, 6)
		if _, err := rand.Read(raw); err != nil {
			return "", fmt.Errorf("generate sandbox id: %w", err)
		}
		id := strings.ToLower(hex.EncodeToString(raw))
		collision := false
		for _, record := range records {
			collision = collision || record.ID == id
		}
		if !collision {
			return id, nil
		}
	}
	return "", errors.New("could not allocate a unique sandbox id")
}

func pruneSandboxHistory(records []sandboxRecord) []sandboxRecord {
	if len(records) <= sandboxHistoryLimit {
		return records
	}
	var active, finished []sandboxRecord
	for _, record := range records {
		if record.Active() {
			active = append(active, record)
			continue
		}
		finished = append(finished, record)
	}
	sort.Slice(finished, func(i, j int) bool { return finished[i].CreatedAt.After(finished[j].CreatedAt) })
	keep := sandboxHistoryLimit - len(active)
	if keep < 0 {
		keep = 0
	}
	if keep > len(finished) {
		keep = len(finished)
	}
	return append(active, finished[:keep]...)
}

func validSandboxStatus(status string) bool {
	switch status {
	case SandboxStatusStarting, SandboxStatusRunning, SandboxStatusExpired, SandboxStatusDestroyed, SandboxStatusFailed:
		return true
	}
	return false
}

func normalizeSandboxNote(note string) string {
	note = strings.Join(strings.Fields(note), " ")
	if len(note) > 240 {
		return note[:240]
	}
	return note
}

func (s Store) loadSandboxFile() (sandboxFile, error) {
	raw, err := os.ReadFile(filepath.Join(s.dir, sandboxesFilename))
	if errors.Is(err, os.ErrNotExist) {
		return sandboxFile{Version: 1}, nil
	}
	if err != nil {
		return sandboxFile{}, fmt.Errorf("read sandbox store: %w", err)
	}
	var file sandboxFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return sandboxFile{}, fmt.Errorf("parse sandbox store: %w", err)
	}
	if file.Version != 1 {
		return sandboxFile{}, errors.New("unsupported sandbox store version")
	}
	for _, record := range file.Sandboxes {
		if !safeID(record.ID) || !IsSandboxJobID(record.JobID) || len(record.TokenDigest) != sha256.Size*2 {
			return sandboxFile{}, errors.New("sandbox store contains an invalid record")
		}
		if _, err := hex.DecodeString(record.TokenDigest); err != nil {
			return sandboxFile{}, errors.New("sandbox store contains an invalid digest")
		}
	}
	return file, nil
}

func (s Store) writeSandboxFile(file sandboxFile) error {
	file.Version = 1
	raw, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("encode sandbox store: %w", err)
	}
	raw = append(raw, '\n')
	temporary, err := os.CreateTemp(s.dir, ".sandboxes-*")
	if err != nil {
		return fmt.Errorf("create temporary sandbox store: %w", err)
	}
	temporaryName := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure temporary sandbox store: %w", err)
	}
	if _, err := temporary.Write(raw); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary sandbox store: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary sandbox store: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary sandbox store: %w", err)
	}
	if err := os.Rename(temporaryName, filepath.Join(s.dir, sandboxesFilename)); err != nil {
		return fmt.Errorf("replace sandbox store: %w", err)
	}
	removeTemporary = false
	return nil
}
