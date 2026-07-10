package pool

import (
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
printf 'nomad nodes:\n'
nomad node status
printf 'nomad jobs:\n'
nomad job status`)
	return runLogged("ssh", "-i", key, target, cmd)
}

func remoteNomadCommand(node Node, body string) string {
	return fmt.Sprintf(`set -euo pipefail
token="$(sudo cat /etc/nomad.d/acl/bootstrap.token 2>/dev/null || sudo cat /etc/nomad.d/bootstrap.token)"
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
