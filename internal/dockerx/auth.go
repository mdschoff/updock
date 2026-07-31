package dockerx

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/registry"
)

// dockerConfig is the subset of ~/.docker/config.json updock understands.
type dockerConfig struct {
	Auths       map[string]dockerAuthEntry `json:"auths"`
	CredsStore  string                     `json:"credsStore"`
	CredHelpers map[string]string          `json:"credHelpers"`
}

type dockerAuthEntry struct {
	Auth     string `json:"auth"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// indexServer is the config key Docker Hub credentials are stored under.
const indexServer = "https://index.docker.io/v1/"

// encodedAuthFor returns the base64-encoded registry auth header for an image
// reference, or "" when no credentials are configured. All failures degrade
// to anonymous access — a wrong credential setup should behave no worse than
// having none.
func encodedAuthFor(imageRef string) string {
	cfg := loadDockerConfig()
	if cfg == nil {
		return ""
	}
	host := registryHostOf(imageRef)

	// Resolution order mirrors the docker CLI: per-registry credential
	// helper, then the default credential store, then plain auths entries.
	if helper, ok := cfg.CredHelpers[host]; ok {
		return encodeAuth(credsFromHelper(helper, host))
	}
	if cfg.CredsStore != "" {
		if auth := encodeAuth(credsFromHelper(cfg.CredsStore, host)); auth != "" {
			return auth
		}
	}
	for key, entry := range cfg.Auths {
		if normalizeRegistryKey(key) != host {
			continue
		}
		user, pass := entry.Username, entry.Password
		if entry.Auth != "" {
			if decoded, err := base64.StdEncoding.DecodeString(entry.Auth); err == nil {
				if u, p, ok := strings.Cut(string(decoded), ":"); ok {
					user, pass = u, p
				}
			}
		}
		return encodeAuth(user, pass, host)
	}
	return ""
}

func loadDockerConfig() *dockerConfig {
	dir := os.Getenv("DOCKER_CONFIG")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		dir = filepath.Join(home, ".docker")
	}
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return nil
	}
	var cfg dockerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		slog.Debug("cannot parse docker config.json", "err", err)
		return nil
	}
	return &cfg
}

// registryHostOf returns the registry hostname credentials should be looked
// up under for an image reference ("docker.io" for Docker Hub images).
func registryHostOf(imageRef string) string {
	named, err := reference.ParseNormalizedNamed(imageRef)
	if err != nil {
		return ""
	}
	return reference.Domain(named)
}

// normalizeRegistryKey maps a config.json auths key to a registry hostname.
// Docker Hub is stored under its legacy index URL; other entries may carry a
// scheme prefix.
func normalizeRegistryKey(key string) string {
	if key == indexServer {
		return "docker.io"
	}
	key = strings.TrimPrefix(key, "https://")
	key = strings.TrimPrefix(key, "http://")
	return strings.TrimSuffix(key, "/")
}

// credsFromHelper asks a docker credential helper (docker-credential-<name>)
// for credentials. Helpers are host binaries; inside updock's scratch
// container they are absent and this quietly returns nothing.
func credsFromHelper(helper, host string) (user, pass, server string) {
	// Docker Hub helpers store credentials under the legacy index URL.
	query := host
	if host == "docker.io" {
		query = indexServer
	}
	cmd := exec.Command("docker-credential-"+helper, "get")
	cmd.Stdin = strings.NewReader(query)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		slog.Debug("credential helper lookup failed", "helper", helper, "registry", host, "err", err)
		return "", "", ""
	}
	var creds struct {
		Username string
		Secret   string
	}
	if err := json.Unmarshal(out.Bytes(), &creds); err != nil {
		return "", "", ""
	}
	return creds.Username, creds.Secret, host
}

// encodeAuth builds the base64 auth header the Engine API expects.
func encodeAuth(user, pass, server string) string {
	if user == "" && pass == "" {
		return ""
	}
	payload, err := json.Marshal(registry.AuthConfig{
		Username:      user,
		Password:      pass,
		ServerAddress: server,
	})
	if err != nil {
		return ""
	}
	return base64.URLEncoding.EncodeToString(payload)
}
