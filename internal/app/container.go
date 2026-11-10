package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Container bootstrap. The image no longer ships a shell/entrypoint script:
// the binary itself prepares /data, writes a default config, drops root, and
// answers Docker's healthcheck.
const (
	containerEnv           = "OPENCODE2API_CONTAINER"
	containerDataDir       = "/data"
	containerExampleConfig = "/app/config.example.json"
	containerUID           = 65532
	containerGID           = 65532
)

// inContainer reports whether the image enabled container bootstrap.
func inContainer() bool { return envBool(containerEnv, false) }

// envString returns a trimmed environment value, or fallback when unset/blank.
func envString(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// envBool parses a boolean environment value, falling back on unset/invalid.
func envBool(key string, fallback bool) bool {
	v, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(key)))
	if err != nil {
		return fallback
	}
	return v
}

// prepareContainer creates the config directory/file when missing, then
// best-effort chowns /data and drops to the distroless/scratch nonroot UID.
// It is a no-op when the process already runs unprivileged.
func prepareContainer(configPath string) error {
	if dir := filepath.Dir(configPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create config directory: %w%s", err, containerWriteHint(dir))
		}
	}

	createdConfig := false
	if _, err := os.Stat(configPath); errors.Is(err, fs.ErrNotExist) {
		// Check writability before reading the example: otherwise an
		// unwritable /data surfaces as "read example config: ...", which sends
		// the operator after the wrong problem. Missing config + unwritable
		// directory is unrecoverable, so this one is fatal.
		if err := requireContainerWritable(filepath.Dir(configPath)); err != nil {
			return err
		}
		if err := writeInitialConfig(configPath, containerExampleConfig); err != nil {
			return err
		}
		createdConfig = true
	} else if err != nil {
		return fmt.Errorf("stat config: %w", err)
	}

	if os.Geteuid() == 0 {
		chownTree(containerDataDir)
		if err := dropContainerPrivileges(); err != nil {
			return fmt.Errorf("drop privileges: %w", err)
		}
	}

	// Verify instead of trusting chownTree: it swallows errors by design
	// (read-only mounts, rootless/ID-mapped storage), and the privilege drop is
	// irreversible. Run this on the unprivileged path too — an existing config
	// with `--user` skips everything above, which is exactly the bind-mount case
	// that would otherwise keep running with no writable /data.
	//
	// Fatal when this run created the config: the probe above ran as root and
	// therefore always passed, so a chown that silently did nothing would leave
	// a root-owned config and a data dir the server UID cannot write — the
	// process would come up on the default password with nothing persisted.
	// A pre-existing config is different: the operator supplied it, and a
	// read-only mount is a legitimate deployment, so that only warns.
	if err := verifyContainerReadOnly(configPath); err != nil {
		if createdConfig {
			return err
		}
		slog.Warn(err.Error())
	}
	return nil
}

// directoryWritable probes for create permission in dir without leaving a file.
func directoryWritable(dir string) error {
	probe, err := os.CreateTemp(dir, ".write-probe-*")
	if err != nil {
		return err
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}

// requireContainerWritable proves the current UID can create files next to the
// config, which is where stats.json, the log, and the models.dev cache also
// live. Failing here names the actual problem instead of surfacing a permission
// error much later from a log rotation or a config save.
func requireContainerWritable(dir string) error {
	if err := directoryWritable(dir); err != nil {
		return fmt.Errorf("container data directory %s is not writable by uid %d: %w%s",
			dir, os.Geteuid(), err, containerWriteHint(dir))
	}
	return nil
}

// verifyContainerReadOnly reports the degradation once at startup instead of
// letting it surface as scattered errors from a log rotation hours later. The
// caller decides whether that is fatal (freshly created config) or a warning
// (operator-supplied config on a read-only mount such as a Kubernetes
// ConfigMap).
func verifyContainerReadOnly(configPath string) error {
	dir := filepath.Dir(configPath)
	if err := directoryWritable(dir); err != nil {
		return fmt.Errorf("container data directory %s is not writable by uid %d; token stats, the log file, and the models.dev cache will not persist: %w%s",
			dir, os.Geteuid(), err, containerWriteHint(dir))
	}
	f, err := os.OpenFile(configPath, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("config file %s is not writable by uid %d; admin panel edits and runtime config saves will not persist: %w",
			configPath, os.Geteuid(), err)
	}
	_ = f.Close()
	return nil
}

// containerHintBody is the single-line remedy text shared by the fatal error
// and the startup warning. It must fit one line: the warning emits it as a
// slog field, and a newline inside a value would be escaped into literal `\n`.
//
// The image has no shell, so a bare "permission denied" is undebuggable: the
// two causes (bind mount owned by the host user, read-only mount) need
// different remedies. A named volume is not affected: Docker seeds it from the
// image's /data, which is already owned by containerUID, so
// `--user 65532:65532` works there. A bind mount keeps the host owner and never
// gets chowned unless the process starts as root, which is exactly the case
// --user skips.
func containerHintBody(dir string) string {
	if os.Geteuid() == 0 {
		return fmt.Sprintf("running as root but %s is not writable — the mount is probably read-only "+
			"(check `:ro` / SELinux labels), or point OPENCODE2API_CONFIG at a writable path", dir)
	}
	return fmt.Sprintf("%s is not writable by uid %d. A bind mount keeps the host owner, so the "+
		"container user cannot create files there. Use a named volume instead of a bind mount, "+
		"pre-chown the host directory to %d:%d, or start without --user so the process begins as "+
		"root and can chown /data before dropping privileges", dir, os.Geteuid(), containerUID, containerGID)
}

// containerWriteHint is containerHintBody formatted for appending to a fatal
// bootstrap error, which is printed to stderr rather than logged.
func containerWriteHint(dir string) string {
	return "\nhint: " + containerHintBody(dir)
}

// writeInitialConfig copies the bundled example, or generates a SOCKS5-enabled
// config when OPENCODE2API_SOCKS5_ADDR is set. Existing files are never touched.
func writeInitialConfig(path, examplePath string) error {
	data, err := initialConfigBytes(examplePath)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write initial config: %w", err)
	}
	return nil
}

func initialConfigBytes(examplePath string) ([]byte, error) {
	addr := strings.TrimSpace(os.Getenv("OPENCODE2API_SOCKS5_ADDR"))
	if addr == "" {
		data, err := os.ReadFile(examplePath)
		if err != nil {
			return nil, fmt.Errorf("read example config: %w", err)
		}
		return data, nil
	}

	name := envString("OPENCODE2API_SOCKS5_NAME", "proxy")
	cfg := map[string]any{
		"model_alias": map[string]string{
			"deepseek-v4-flash": "deepseek-v4-flash-free",
			"mimo-v2.5":         "mimo-v2.5-free",
			"ling-3.0-flash":    "ling-3.0-flash-free",
			"nemotron-3-ultra":  "nemotron-3-ultra-free",
			"north-mini-code":   "north-mini-code-free",
			"laguna-s-2.1":      "laguna-s-2.1-free",
		},
		"reasoning_effort_map": map[string]string{
			"minimal": "low",
			"medium":  "medium",
			"high":    "high",
		},
		"force_disable_thinking": false,
		"socks5_proxies": []map[string]string{{
			"name":     name,
			"addr":     addr,
			"username": "",
			"password": "",
		}},
		"active_socks5": addr,
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal initial config: %w", err)
	}
	return append(data, '\n'), nil
}

// chownTree mirrors the old `chown -R opencode2api:opencode2api /data` and
// intentionally ignores errors: bind mounts may be read-only or use IDs the
// container cannot change.
func chownTree(root string) {
	_ = filepath.WalkDir(root, func(path string, _ fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		_ = os.Lchown(path, containerUID, containerGID)
		return nil
	})
}

// probeHealth is used by `-healthcheck`, replacing the wget-based Docker
// healthcheck now that the image has no shell or busybox.
func probeHealth(port string) error {
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort("127.0.0.1", port) + "/health")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}
	return nil
}
