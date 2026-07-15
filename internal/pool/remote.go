package pool

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

func ApplyControlPlaneBundle(node Node, localDir, remoteDir string) error {
	if node.PublicIP == "" || node.SSHUser == "" || node.SSHKey == "" {
		return fmt.Errorf("control-plane node %q is missing SSH connection fields", node.Name)
	}
	key, err := expandHome(node.SSHKey)
	if err != nil {
		return err
	}
	target := fmt.Sprintf("%s@%s", node.SSHUser, node.PublicIP)

	prep := fmt.Sprintf("rm -rf %[1]s && mkdir -p %[1]s", remoteDir)
	if err := runLogged("ssh", "-i", key, target, prep); err != nil {
		return err
	}
	if err := runLogged("scp", "-i", key, "-r", localDir+"/.", target+":"+remoteDir+"/"); err != nil {
		return err
	}
	if err := runLogged("ssh", "-i", key, target, "cd "+remoteDir+" && ./bootstrap-control-plane.sh"); err != nil {
		return err
	}
	return nil
}

func JoinWorker(cfg Config, workerName string, localDir string) error {
	control, ok := ControlPlaneNode(cfg)
	if !ok {
		return fmt.Errorf("config has no control-plane node")
	}
	worker, ok := findNode(cfg, workerName)
	if !ok {
		return fmt.Errorf("unknown node %q", workerName)
	}
	if worker.Role == "control-plane" {
		return fmt.Errorf("node %q is the control plane, not a worker", workerName)
	}
	if err := requireNodeSSH(control); err != nil {
		return err
	}
	if err := requireNodeSSH(worker); err != nil {
		return err
	}
	if control.OverlayIP == "" {
		return fmt.Errorf("control-plane node %q is missing overlay_ip", control.Name)
	}
	if worker.OverlayIP == "" {
		return fmt.Errorf("worker node %q is missing overlay_ip", worker.Name)
	}

	controlKey, err := expandHome(control.SSHKey)
	if err != nil {
		return err
	}
	workerKey, err := expandHome(worker.SSHKey)
	if err != nil {
		return err
	}
	controlTarget := fmt.Sprintf("%s@%s", control.SSHUser, control.PublicIP)
	workerTarget := fmt.Sprintf("%s@%s", worker.SSHUser, worker.PublicIP)

	fmt.Printf("checking non-interactive sudo on %s\n", worker.Name)
	if err := runLogged("ssh", "-i", workerKey, workerTarget, "sudo -n true"); err != nil {
		return fmt.Errorf("worker %q needs passwordless sudo for poolctl web join; run: echo '%s ALL=(ALL) NOPASSWD:ALL' | sudo tee /etc/sudoers.d/90-poolctl-%s && sudo chmod 0440 /etc/sudoers.d/90-poolctl-%s: %w", worker.Name, worker.SSHUser, worker.SSHUser, worker.SSHUser, err)
	}

	fmt.Printf("preparing worker WireGuard key on %s\n", worker.Name)
	workerPub, err := runOutput("ssh", "-i", workerKey, workerTarget, workerPrepareWireGuardCommand())
	if err != nil {
		return err
	}
	workerPub = extractWireGuardKey(workerPub)
	if workerPub == "" {
		return fmt.Errorf("worker %q did not return a WireGuard public key", worker.Name)
	}

	fmt.Printf("reading control-plane WireGuard key from %s\n", control.Name)
	controlPub, err := runOutput("ssh", "-i", controlKey, controlTarget, "sudo cat /etc/wireguard/poolctl-control.key | wg pubkey")
	if err != nil {
		return err
	}
	controlPub = strings.TrimSpace(controlPub)
	if controlPub == "" {
		return fmt.Errorf("control plane %q did not return a WireGuard public key", control.Name)
	}

	if err := os.RemoveAll(localDir); err != nil {
		return err
	}
	files, err := RenderWorkerJoin(control, worker, controlPub)
	if err != nil {
		return err
	}
	if err := WriteRendered(localDir, files); err != nil {
		return err
	}

	remoteTLSDir := "/tmp/poolctl-join-" + shellSafeName(worker.Name) + "-tls"
	fmt.Printf("generating Nomad client certificate on %s\n", control.Name)
	if err := runLogged("ssh", "-i", controlKey, controlTarget, controlRenderWorkerTLSCommand(worker, remoteTLSDir)); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(localDir, "tls"), 0o755); err != nil {
		return err
	}
	if err := runLogged("scp", "-i", controlKey, "-r", controlTarget+":"+remoteTLSDir+"/.", filepath.Join(localDir, "tls")+"/"); err != nil {
		return err
	}

	remoteDir := "~/poolctl-worker-join"
	fmt.Printf("copying worker bootstrap bundle to %s\n", worker.Name)
	if err := runLogged("ssh", "-i", workerKey, workerTarget, "rm -rf "+remoteDir+" && mkdir -p "+remoteDir); err != nil {
		return err
	}
	if err := runLogged("scp", "-i", workerKey, "-r", localDir+"/.", workerTarget+":"+remoteDir+"/"); err != nil {
		return err
	}

	fmt.Printf("running worker bootstrap on %s\n", worker.Name)
	if err := runLogged("ssh", "-i", workerKey, workerTarget, "cd "+remoteDir+" && POOLCTL_SKIP_WG_WAIT=1 ./bootstrap-worker.sh"); err != nil {
		fmt.Printf("worker bootstrap failed; collecting control-plane WireGuard diagnostics for %s\n", worker.Name)
		_ = runLogged("ssh", "-i", controlKey, controlTarget, controlWireGuardDiagnosticsCommand(worker))
		return err
	}

	fmt.Printf("reading active worker WireGuard key from %s\n", worker.Name)
	activeWorkerPub, err := runOutput("ssh", "-i", workerKey, workerTarget, "sudo wg show wg0 public-key")
	if err != nil {
		return err
	}
	activeWorkerPub = extractWireGuardKey(activeWorkerPub)
	if activeWorkerPub == "" {
		return fmt.Errorf("worker %q did not return an active wg0 public key", worker.Name)
	}
	if activeWorkerPub != workerPub {
		fmt.Printf("worker key changed after bootstrap; using active wg0 key for Oracle peer\n")
	}

	fmt.Printf("adding WireGuard peer on %s for %s\n", control.Name, worker.Name)
	if err := runLogged("ssh", "-i", controlKey, controlTarget, controlAddPeerCommand(worker, activeWorkerPub)); err != nil {
		return err
	}
	fmt.Printf("allowing Nomad overlay traffic on %s\n", control.Name)
	if err := runLogged("ssh", "-i", controlKey, controlTarget, controlAllowNomadOverlayCommand()); err != nil {
		return err
	}

	fmt.Printf("verifying worker WireGuard reachability on %s\n", worker.Name)
	if err := runLogged("ssh", "-i", workerKey, workerTarget, workerFinalizeJoinCommand(control)); err != nil {
		fmt.Printf("worker verification failed; collecting control-plane WireGuard diagnostics for %s\n", worker.Name)
		_ = runLogged("ssh", "-i", controlKey, controlTarget, controlWireGuardDiagnosticsCommand(worker))
		return err
	}

	fmt.Println("worker join applied; checking control-plane status")
	if err := CheckControlPlaneStatus(control); err != nil {
		return err
	}
	if err := verifyNomadWorker(control, worker); err != nil {
		fmt.Printf("worker is reachable over WireGuard but Nomad did not register it; collecting worker diagnostics\n")
		_ = runLogged("ssh", "-i", workerKey, workerTarget, workerNomadDiagnosticsCommand(control, worker))
		return err
	}
	return nil
}

func DeployAppJob(node Node, app App, localJobPath, remoteDir string) error {
	if node.PublicIP == "" || node.SSHUser == "" || node.SSHKey == "" {
		return fmt.Errorf("control-plane node %q is missing SSH connection fields", node.Name)
	}
	if node.OverlayIP == "" {
		return fmt.Errorf("control-plane node %q is missing overlay_ip", node.Name)
	}
	key, err := expandHome(node.SSHKey)
	if err != nil {
		return err
	}
	target := fmt.Sprintf("%s@%s", node.SSHUser, node.PublicIP)
	remoteDir = normalizeRemoteHome(remoteDir)
	remoteJob := path.Join(remoteDir, path.Base(localJobPath))

	if err := runLogged("ssh", "-i", key, target, "mkdir -p "+shellQuote(remoteDir)); err != nil {
		return err
	}
	if err := runLogged("scp", "-i", key, localJobPath, target+":"+remoteJob); err != nil {
		return err
	}

	cmd := remoteNomadCommand(node, fmt.Sprintf("nomad job run %s", shellQuote(remoteJob)))
	if err := runLogged("ssh", "-i", key, target, cmd); err != nil {
		return err
	}
	fmt.Printf("deployed app %s from %s\n", app.Name, localJobPath)
	return nil
}

func findNode(cfg Config, name string) (Node, bool) {
	for _, node := range cfg.Nodes {
		if node.Name == name {
			return node, true
		}
	}
	return Node{}, false
}

func requireNodeSSH(node Node) error {
	if node.PublicIP == "" || node.SSHUser == "" || node.SSHKey == "" {
		return fmt.Errorf("node %q is missing SSH connection fields", node.Name)
	}
	return nil
}

func workerPrepareWireGuardCommand() string {
	return `set -euo pipefail
sudo -n apt-get update >&2
sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y wireguard >&2
sudo -n install -d -m 0700 /etc/wireguard
if ! sudo -n test -f /etc/wireguard/poolctl-worker.key; then
  wg genkey | sudo -n tee /etc/wireguard/poolctl-worker.key >/dev/null
  sudo -n chmod 0600 /etc/wireguard/poolctl-worker.key
fi
sudo -n cat /etc/wireguard/poolctl-worker.key | wg pubkey`
}

func controlAddPeerCommand(worker Node, workerPub string) string {
	return fmt.Sprintf(`set -euo pipefail
sudo install -d -m 0700 /etc/wireguard
tmp="$(mktemp)"
sudo awk -v peer_comment="# %s" '
  function flush_pending() {
    if (pending != "") {
      print pending
      pending = ""
    }
  }
  skip && $0 == "[Peer]" {
    skip = 0
    pending = $0
    next
  }
  skip {
    next
  }
  $0 == "[Peer]" {
    flush_pending()
    pending = $0
    next
  }
  pending != "" {
    if ($0 == peer_comment) {
      skip = 1
      pending = ""
      next
    }
    print pending
    pending = ""
    print
    next
  }
  { print }
  END {
    if (!skip) {
      flush_pending()
    }
  }
' /etc/wireguard/wg0.conf > "$tmp"
sudo install -m 0600 "$tmp" /etc/wireguard/wg0.conf
rm -f "$tmp"
sudo tee -a /etc/wireguard/wg0.conf >/dev/null <<WGPEER

[Peer]
# %s
PublicKey = %s
AllowedIPs = %s/32
PersistentKeepalive = 25
WGPEER
sudo wg set wg0 peer %s allowed-ips %s/32 persistent-keepalive 25
sudo systemctl restart wg-quick@wg0`, worker.Name, worker.Name, workerPub, worker.OverlayIP, shellQuote(workerPub), worker.OverlayIP)
}

func controlRenderWorkerTLSCommand(worker Node, remoteDir string) string {
	return fmt.Sprintf(`set -euo pipefail
tmp=%s
sudo rm -rf "$tmp"
sudo install -d -m 0700 "$tmp"
sudo sh -c 'cat > "$1/client.cnf"' sh "$tmp" <<EOF
[req]
default_bits = 2048
prompt = no
default_md = sha256
req_extensions = req_ext
distinguished_name = dn

[dn]
CN = client.global.nomad

[req_ext]
subjectAltName = @alt_names

[alt_names]
DNS.1 = client.global.nomad
DNS.2 = %s
IP.1 = %s
EOF
sudo openssl genrsa -out "$tmp/global-client-nomad-key.pem" 2048
sudo openssl req -new -key "$tmp/global-client-nomad-key.pem" -out "$tmp/global-client-nomad.csr" -config "$tmp/client.cnf"
sudo openssl x509 -req -in "$tmp/global-client-nomad.csr" \
  -CA /etc/nomad.d/tls/nomad-agent-ca.pem \
  -CAkey /etc/nomad.d/tls/nomad-agent-ca-key.pem \
  -CAcreateserial -out "$tmp/global-client-nomad.pem" -days 825 -sha256 \
  -extensions req_ext -extfile "$tmp/client.cnf"
sudo cp /etc/nomad.d/tls/nomad-agent-ca.pem "$tmp/nomad-agent-ca.pem"
sudo chmod 0644 "$tmp/nomad-agent-ca.pem" "$tmp/global-client-nomad.pem"
sudo chmod 0640 "$tmp/global-client-nomad-key.pem"
sudo chown -R %s:%s "$tmp"`, shellQuote(remoteDir), worker.Name, worker.OverlayIP, shellQuote(worker.SSHUser), shellQuote(worker.SSHUser))
}

func controlAllowNomadOverlayCommand() string {
	return `set -euo pipefail
if command -v ufw >/dev/null 2>&1; then
  sudo ufw allow in on wg0 from 10.44.0.0/24 to any port 4646:4648 proto tcp
  sudo ufw allow in on wg0 from 10.44.0.0/24 to any port 20000:32000 proto tcp
fi
for port in 4646 4647 4648; do
  if ! sudo iptables -C INPUT -i wg0 -p tcp -s 10.44.0.0/24 --dport "$port" -m comment --comment "poolctl-nomad-overlay-$port" -j ACCEPT 2>/dev/null; then
    reject_line="$(sudo iptables -L INPUT --line-numbers -n | awk '$2 == "REJECT" { print $1; exit }')"
    if [ -n "$reject_line" ]; then
      sudo iptables -I INPUT "$reject_line" -i wg0 -p tcp -s 10.44.0.0/24 --dport "$port" -m comment --comment "poolctl-nomad-overlay-$port" -j ACCEPT
    else
      sudo iptables -A INPUT -i wg0 -p tcp -s 10.44.0.0/24 --dport "$port" -m comment --comment "poolctl-nomad-overlay-$port" -j ACCEPT
    fi
  fi
done
if command -v netfilter-persistent >/dev/null 2>&1; then
  sudo netfilter-persistent save || true
elif command -v iptables-save >/dev/null 2>&1 && sudo test -d /etc/iptables; then
  sudo sh -c 'iptables-save > /etc/iptables/rules.v4'
fi`
}

func workerFinalizeJoinCommand(control Node) string {
	return fmt.Sprintf(`set -euo pipefail
sudo systemctl restart wg-quick@wg0
for _ in $(seq 1 30); do
  if ping -c 1 -W 1 %s >/dev/null 2>&1; then
    sudo systemctl restart nomad
    exit 0
  fi
  sleep 2
done
echo "---- worker wg show ----"
sudo wg show || true
echo "---- worker wg0 config ----"
sudo sed -n '1,120p' /etc/wireguard/wg0.conf | sed 's/PrivateKey = .*/PrivateKey = <redacted>/' || true
echo "---- worker route to control overlay ----"
ip route get %s || true
exit 1`, control.OverlayIP, control.OverlayIP)
}

func verifyNomadWorker(control, worker Node) error {
	key, err := expandHome(control.SSHKey)
	if err != nil {
		return err
	}
	target := fmt.Sprintf("%s@%s", control.SSHUser, control.PublicIP)
	cmd := remoteNomadCommand(control, fmt.Sprintf(`for _ in $(seq 1 30); do
  if nomad node status | awk '{print $4}' | grep -Fx %s >/dev/null 2>&1; then
    nomad node status
    exit 0
  fi
  sleep 2
done
nomad node status
exit 1`, shellQuote(worker.Name)))
	return runLogged("ssh", "-i", key, target, cmd)
}

func workerNomadDiagnosticsCommand(control, worker Node) string {
	return fmt.Sprintf(`set -euo pipefail
echo "---- worker systemd nomad ----"
sudo systemctl status nomad --no-pager -l || true
echo "---- worker nomad journal ----"
sudo journalctl -u nomad -n 200 --no-pager || true
echo "---- worker listeners ----"
sudo ss -ltnp | grep -E ':(4646|4647|4648)\b' || true
echo "---- worker nomad config ----"
sudo sed -n '1,180p' /etc/nomad.d/client.hcl || true
echo "---- worker agent token files ----"
sudo find /opt/nomad /etc/nomad.d -maxdepth 4 -type f \( -name '*token*' -o -name 'acl_tokens.json' \) -print 2>/dev/null || true
echo "---- worker can reach control Nomad HTTP ----"
curl -k -sS -o /tmp/poolctl-nomad-worker-check -w '%%{http_code}\n' https://%s:4646/v1/status/leader || true
sudo sed -n '1,20p' /tmp/poolctl-nomad-worker-check 2>/dev/null || true`, control.OverlayIP)
}

func controlWireGuardDiagnosticsCommand(worker Node) string {
	return fmt.Sprintf(`set -euo pipefail
echo "---- control wg show ----"
sudo wg show || true
echo "---- control wg0 peer block for %s ----"
sudo awk -v peer_comment="# %s" '
  $0 == "[Peer]" { block = $0 "\n"; in_block = 1; wanted = 0; next }
  in_block {
    block = block $0 "\n"
    if ($0 == peer_comment) wanted = 1
    next
  }
  END {
    if (wanted) print block
  }
' /etc/wireguard/wg0.conf || true
echo "---- control route to worker overlay ----"
ip route get %s || true
echo "---- control ping worker overlay ----"
ping -c 3 -W 1 %s || true`, worker.Name, worker.Name, worker.OverlayIP, worker.OverlayIP)
}

func CheckControlPlaneStatus(node Node) error {
	if node.PublicIP == "" || node.SSHUser == "" || node.SSHKey == "" {
		return fmt.Errorf("control-plane node %q is missing SSH connection fields", node.Name)
	}
	if node.OverlayIP == "" {
		return fmt.Errorf("control-plane node %q is missing overlay_ip", node.Name)
	}
	key, err := expandHome(node.SSHKey)
	if err != nil {
		return err
	}
	target := fmt.Sprintf("%s@%s", node.SSHUser, node.PublicIP)
	cmd := remoteNomadCommand(node, `printf 'systemd: '
systemctl is-active nomad traefik wg-quick@wg0 | paste -sd ',' - || true
printf 'listeners:\n'
sudo ss -ltnp | grep -E ':(80|443|4646|4647|4648)\b' || true
printf 'ufw:\n'
sudo ufw status verbose || true
printf 'nomad nodes:\n'
nomad node status
printf 'nomad jobs:\n'
nomad job status
printf 'sample-api job detail:\n'
nomad job status sample-api || true
printf 'sample-api allocations:\n'
nomad job allocs sample-api || true
printf 'nomad services api:\n'
if [ -n "$NOMAD_TOKEN" ]; then
  printf 'token: present\n'
else
  printf 'token: missing\n'
fi
tmp_body="$(mktemp)"
code="$(curl -fsS -o "$tmp_body" -w '%{http_code}' --cacert /etc/nomad.d/tls/nomad-agent-ca.pem -H "X-Nomad-Token: $NOMAD_TOKEN" "$NOMAD_ADDR/v1/services" 2>/dev/null || true)"
printf 'GET /v1/services -> %s\n' "$code"
if [ "$code" != "200" ]; then
  sed -n '1,20p' "$tmp_body" || true
fi
rm -f "$tmp_body"
printf 'traefik config:\n'
if sudo grep -q '__NOMAD_TOKEN__' /etc/traefik/traefik.yml 2>/dev/null; then
  printf 'still contains token placeholder\n'
else
  printf 'token rendered into /etc/traefik/traefik.yml\n'
fi
sudo grep -n 'token:' /etc/traefik/traefik.yml 2>/dev/null | sed 's/token:.*/token: <redacted>/' || true
printf 'local ingress smoke:\n'
curl -fsS -H 'Host: sample-api.pool.test' http://127.0.0.1/ | head -20 || true
printf 'private-interface ingress smoke:\n'
private_ip="$(ip -4 -o addr show scope global | awk '$2 != "wg0" && $2 != "docker0" { split($4, a, "/"); print a[1]; exit }')"
printf 'private ip: %s\n' "$private_ip"
if [ -n "$private_ip" ]; then
  curl -fsS -H 'Host: sample-api.pool.test' "http://$private_ip/" | head -20 || true
fi
printf 'public-interface hint:\n'
printf 'If local/private smoke works but public curl fails, check OCI NSG/security-list ingress for TCP 80/443 on the instance VNIC/subnet.\n'
printf 'traefik recent logs:\n'
sudo journalctl -u traefik --since '10 minutes ago' --no-pager || true
printf 'nomad recent logs:\n'
sudo journalctl -u nomad --since '10 minutes ago' --no-pager || true`)
	return runLogged("ssh", "-i", key, target, cmd)
}

func ApplyNodeSchedulerAction(cfg Config, action, nodeName string) error {
	control, ok := ControlPlaneNode(cfg)
	if !ok {
		return fmt.Errorf("config has no control-plane node")
	}
	if _, ok := findNode(cfg, nodeName); !ok {
		return fmt.Errorf("unknown node %q", nodeName)
	}
	if err := requireNodeSSH(control); err != nil {
		return err
	}

	var command string
	switch action {
	case "freeze":
		command = `nomad node eligibility -disable "$node_id"`
	case "unfreeze":
		command = `nomad node eligibility -enable "$node_id"`
	case "drain":
		command = `nomad node drain -enable -detach -yes -m "poolctl local web drain" "$node_id"`
	case "cancel-drain":
		command = `nomad node drain -disable -keep-ineligible -yes -m "poolctl local web drain cancelled" "$node_id"`
	default:
		return fmt.Errorf("unknown node scheduler action %q", action)
	}

	key, err := expandHome(control.SSHKey)
	if err != nil {
		return err
	}
	target := fmt.Sprintf("%s@%s", control.SSHUser, control.PublicIP)
	body := fmt.Sprintf(`node_name=%s
node_id="$(nomad node status -json | python3 -c 'import json,sys; name=sys.argv[1]; nodes=json.load(sys.stdin); print(next((n["ID"] for n in nodes if n["Name"] == name), ""))' "$node_name")"
if [ -z "$node_id" ]; then
  echo "Nomad node not registered: $node_name" >&2
  exit 1
fi
%s
nomad node status "$node_id"`, shellQuote(nodeName), command)
	return runLogged("ssh", "-i", key, target, remoteNomadCommand(control, body))
}

func remoteNomadCommand(node Node, body string) string {
	return fmt.Sprintf(`set -euo pipefail
token="$(for file in /var/lib/poolctl/nomad-acl/bootstrap.token /etc/nomad.d/acl/bootstrap.token /etc/nomad.d/bootstrap.token; do if sudo test -s "$file"; then sudo awk 'NF { print; exit }' "$file"; break; fi; done)"
sudo env NOMAD_ADDR=%s NOMAD_CACERT=/etc/nomad.d/tls/nomad-agent-ca.pem NOMAD_TOKEN="$token" sh -lc %s`, shellQuote("https://"+node.OverlayIP+":4646"), shellQuote(body))
}

func runLogged(name string, args ...string) error {
	fmt.Printf("+ %s %s\n", name, strings.Join(maskArgs(args), " "))
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func runOutput(name string, args ...string) (string, error) {
	fmt.Printf("+ %s %s\n", name, strings.Join(maskArgs(args), " "))
	cmd := exec.Command(name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	err := cmd.Run()
	return out.String(), err
}

func maskArgs(args []string) []string {
	masked := make([]string, len(args))
	copy(masked, args)
	for i := 0; i < len(masked)-1; i++ {
		if masked[i] == "-i" {
			masked[i+1] = "<ssh-key>"
		}
	}
	return masked
}

func expandHome(path string) (string, error) {
	if path == "~" {
		return os.UserHomeDir()
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
	}
	return path, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func normalizeRemoteHome(path string) string {
	if path == "~" || path == "$HOME" {
		return "."
	}
	if strings.HasPrefix(path, "~/") {
		return strings.TrimPrefix(path, "~/")
	}
	if strings.HasPrefix(path, "$HOME/") {
		return strings.TrimPrefix(path, "$HOME/")
	}
	return path
}

func shellSafeName(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('-')
	}
	if b.Len() == 0 {
		return "node"
	}
	return b.String()
}

func extractWireGuardKey(raw string) string {
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if len(line) != 44 || !strings.HasSuffix(line, "=") {
			continue
		}
		ok := true
		for _, r := range line {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '+' || r == '/' || r == '=' {
				continue
			}
			ok = false
			break
		}
		if ok {
			return line
		}
	}
	return ""
}
