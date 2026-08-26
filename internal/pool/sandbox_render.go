package pool

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// workspaceCapabilities is the smallest capability set that still lets apt and
// dpkg work inside the container. Everything genuinely dangerous to a host --
// SYS_ADMIN, SYS_MODULE, SYS_PTRACE, SYS_TIME, SYS_BOOT, SYS_RAWIO, NET_ADMIN,
// NET_RAW, MKNOD, AUDIT_CONTROL -- stays dropped in every profile.
var workspaceCapabilities = []string{
	"chown",
	"dac_override",
	"fowner",
	"fsetid",
	"kill",
	"setgid",
	"setuid",
	"setfcap",
}

// RenderSandboxJob renders the Nomad job for one sandbox partition.
//
// The caller never supplies HCL. Every field is validated first and the
// hardened stanzas below are not configurable through the API, so a sandbox
// cannot be talked into requesting privileges, host volumes, host networking,
// Traefik routes, or a placement outside its enrolled node.
func RenderSandboxJob(cfg Config, sandbox Sandbox) (RenderedFile, error) {
	sandbox, err := sandboxWithDefaults(sandbox)
	if err != nil {
		return RenderedFile{}, err
	}
	if sandbox.ID == "" || !safeID(sandbox.ID) {
		return RenderedFile{}, errors.New("sandbox is missing a valid id")
	}
	jobID := SandboxJobID(sandbox.ID)
	if sandbox.JobID != "" && sandbox.JobID != jobID {
		return RenderedFile{}, fmt.Errorf("sandbox job id %q does not match sandbox id %q", sandbox.JobID, sandbox.ID)
	}
	node, ok := cfg.FindNode(sandbox.Node)
	if !ok {
		return RenderedFile{}, fmt.Errorf("sandbox targets unknown node %q", sandbox.Node)
	}
	if node.Role == "control-plane" {
		return RenderedFile{}, errors.New("control-plane nodes cannot host sandboxes")
	}
	if _, ok := cfg.FindApp(jobID); ok {
		return RenderedFile{}, fmt.Errorf("sandbox job name %q collides with a managed app", jobID)
	}

	expires := sandbox.ExpiresAt
	if expires.IsZero() {
		expires = sandbox.CreatedAt.Add(time.Duration(sandbox.TTLSeconds) * time.Second)
	}

	networkMode := "none"
	if sandbox.Network == SandboxNetworkEgress {
		networkMode = SandboxDockerNetwork
	}
	readonlyRootfs := sandbox.Profile == SandboxProfileStrict

	var b strings.Builder
	fmt.Fprintf(&b, "job %s {\n", strconv.Quote(jobID))
	b.WriteString("  datacenters = [\"pool\"]\n")
	b.WriteString("  type        = \"batch\"\n")
	// Sandboxes yield to every managed application during scheduling.
	b.WriteString("  priority    = 10\n\n")

	b.WriteString("  meta {\n")
	b.WriteString("    poolctl_kind       = \"sandbox\"\n")
	fmt.Fprintf(&b, "    poolctl_sandbox_id = %s\n", strconv.Quote(sandbox.ID))
	fmt.Fprintf(&b, "    poolctl_profile    = %s\n", strconv.Quote(sandbox.Profile))
	fmt.Fprintf(&b, "    poolctl_network    = %s\n", strconv.Quote(sandbox.Network))
	fmt.Fprintf(&b, "    poolctl_expires_at = %s\n", strconv.Quote(expires.UTC().Format(time.RFC3339)))
	b.WriteString("  }\n\n")

	// Placement is pinned to the enrolled node and to Linux ARM64, which is the
	// only architecture the current pool runs.
	fmt.Fprintf(&b, `  constraint {
    attribute = "$${node.unique.name}"
    operator  = "="
    value     = %s
  }

  constraint {
    attribute = "$${attr.kernel.name}"
    operator  = "="
    value     = "linux"
  }

  constraint {
    attribute = "$${attr.cpu.arch}"
    operator  = "="
    value     = "arm64"
  }

`, strconv.Quote(sandbox.Node))

	b.WriteString("  group \"sandbox\" {\n")
	b.WriteString("    count = 1\n\n")
	b.WriteString("    restart {\n      attempts = 0\n      mode     = \"fail\"\n    }\n\n")
	b.WriteString("    reschedule {\n      attempts  = 0\n      unlimited = false\n    }\n\n")
	fmt.Fprintf(&b, "    ephemeral_disk {\n      size    = %d\n      sticky  = false\n      migrate = false\n    }\n\n", sandbox.DiskMB)

	fmt.Fprintf(&b, "    task %s {\n", strconv.Quote(SandboxTaskName))
	b.WriteString("      driver       = \"docker\"\n")
	b.WriteString("      kill_timeout = \"10s\"\n\n")

	// No workload identity is exposed to the task, so nothing inside the
	// sandbox can authenticate to Nomad's task API as this allocation.
	b.WriteString("      identity {\n        env  = false\n        file = false\n      }\n\n")

	b.WriteString("      config {\n")
	fmt.Fprintf(&b, "        image           = %s\n", strconv.Quote(sandbox.Image))
	fmt.Fprintf(&b, "        network_mode    = %s\n", strconv.Quote(networkMode))
	b.WriteString("        privileged      = false\n")
	b.WriteString("        init            = true\n")
	b.WriteString("        ipc_mode        = \"private\"\n")
	fmt.Fprintf(&b, "        pids_limit      = %d\n", sandbox.PIDsLimit)
	fmt.Fprintf(&b, "        readonly_rootfs = %t\n", readonlyRootfs)
	b.WriteString("        cap_drop        = [\"ALL\"]\n")
	if sandbox.Profile == SandboxProfileWorkspace {
		fmt.Fprintf(&b, "        cap_add         = [%s]\n", quotedList(workspaceCapabilities))
	}
	b.WriteString("        security_opt    = [\"no-new-privileges\"]\n")
	b.WriteString("        work_dir        = \"/workspace\"\n")
	b.WriteString("        command         = \"/bin/sleep\"\n")
	fmt.Fprintf(&b, "        args            = [%s]\n", strconv.Quote(strconv.Itoa(sandbox.TTLSeconds)))
	if sandbox.Network == SandboxNetworkEgress {
		// Public resolvers only: the sandbox must not be able to query a
		// host-local or pool-internal resolver.
		b.WriteString("        dns_servers     = [\"1.1.1.1\", \"9.9.9.9\"]\n")
	}
	b.WriteString("\n")
	// Both work areas are size-capped tmpfs in every profile. Nomad's
	// ephemeral_disk value is a scheduling reservation, not an enforced limit,
	// so the writable areas an agent actually uses are bounded here instead.
	half := int64(sandbox.DiskMB) * 1024 * 1024 / 2
	fmt.Fprintf(&b, "        mount {\n          type   = \"tmpfs\"\n          target = \"/tmp\"\n          tmpfs_options {\n            size = %d\n          }\n        }\n\n", half)
	fmt.Fprintf(&b, "        mount {\n          type   = \"tmpfs\"\n          target = \"/workspace\"\n          tmpfs_options {\n            size = %d\n          }\n        }\n\n", half)
	b.WriteString("        logging {\n          type = \"json-file\"\n\n          config {\n            max-size = \"8m\"\n            max-file = \"2\"\n          }\n        }\n\n")
	fmt.Fprintf(&b, "        ulimit {\n          nofile = \"1024:2048\"\n          nproc  = \"%d:%d\"\n        }\n", sandbox.PIDsLimit, sandbox.PIDsLimit)
	b.WriteString("      }\n\n")

	b.WriteString("      env {\n")
	b.WriteString("        HOME            = \"/workspace\"\n")
	b.WriteString("        DEBIAN_FRONTEND = \"noninteractive\"\n")
	b.WriteString("        POOLCTL_SANDBOX = \"1\"\n")
	fmt.Fprintf(&b, "        POOLCTL_PROFILE = %s\n", strconv.Quote(sandbox.Profile))
	b.WriteString("      }\n\n")

	fmt.Fprintf(&b, "      resources {\n        cpu    = %d\n        memory = %d\n      }\n", sandbox.CPU, sandbox.MemoryMB)
	b.WriteString("    }\n")
	b.WriteString("  }\n")
	b.WriteString("}\n")

	return RenderedFile{
		Path:    filepath.Join("nomad", "jobs", jobID+".nomad.hcl"),
		Content: b.String(),
	}, nil
}

func quotedList(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, strconv.Quote(value))
	}
	return strings.Join(quoted, ", ")
}
