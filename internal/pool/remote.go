package pool

import (
	"fmt"
	"os"
	"os/exec"
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
