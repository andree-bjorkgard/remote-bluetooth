package client

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// SecretEnv is the environment variable that holds the shared secret.
const SecretEnv = "REMOTE_BLUETOOTH_SECRET"

// SecretPath returns where the shared secret is stored:
// $XDG_CONFIG_HOME/remote-bluetooth/secret, usually
// ~/.config/remote-bluetooth/secret.
func SecretPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "remote-bluetooth", "secret"), nil
}

// LoadSecret returns the shared secret from $REMOTE_BLUETOOTH_SECRET or,
// when that is unset, from the file at SecretPath.
func LoadSecret() (string, error) {
	if s := strings.TrimSpace(os.Getenv(SecretEnv)); s != "" {
		return s, nil
	}
	path, err := SecretPath()
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("no secret: set $%s or run `remote-bluetooth gen-secret` (writes %s)", SecretEnv, path)
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// SaveSecret writes secret to SecretPath, readable only by the user.
func SaveSecret(secret string) (string, error) {
	path, err := SecretPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, []byte(secret+"\n"), 0o600)
}

// ServersEnv is the environment variable conventionally holding extra
// servers for Config.Servers, comma separated.
const ServersEnv = "REMOTE_BLUETOOTH_SERVERS"

// DefaultPort is the port servers listen on unless configured otherwise.
const DefaultPort = 8829

// ParseServers splits a comma separated list of host[:port] servers, as in
// $REMOTE_BLUETOOTH_SERVERS, adding DefaultPort where no port is given.
func ParseServers(list string) []string {
	var servers []string
	for _, s := range strings.Split(list, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(s); err != nil {
			s = net.JoinHostPort(strings.Trim(s, "[]"), strconv.Itoa(DefaultPort))
		}
		servers = append(servers, s)
	}
	return servers
}
