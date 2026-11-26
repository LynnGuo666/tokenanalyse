package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"tokenanalyse/internal/model"
)

type Identity struct {
	DeviceID string
	Name     string
}

func LoadIdentity(dbPath, name string) (Identity, error) {
	idPath := dbPath + ".device-id"
	deviceID, err := os.ReadFile(idPath)
	if err != nil || len(deviceID) == 0 {
		buffer := make([]byte, 16)
		if _, err := rand.Read(buffer); err != nil {
			return Identity{}, err
		}
		generated := hex.EncodeToString(buffer)
		if err := os.WriteFile(idPath, []byte(generated), 0o600); err != nil {
			return Identity{}, err
		}
		deviceID = []byte(generated)
	}
	if name == "" {
		name = hostname()
	}
	return Identity{DeviceID: string(deviceID), Name: name}, nil
}

func (i Identity) Heartbeat(version string) model.DeviceHeartbeat {
	return model.DeviceHeartbeat{DeviceID: i.DeviceID, Name: i.Name,
		Platform: model.Platform(runtime.GOOS), Arch: runtime.GOARCH,
		AgentVersion: version, SentAt: time.Now().UTC()}
}

func (i Identity) Register() model.Device {
	now := time.Now().UTC()
	return model.Device{DeviceID: i.DeviceID, Name: i.Name,
		Platform: model.Platform(runtime.GOOS), Arch: runtime.GOARCH,
		AgentVersion: version(), FirstSeenAt: now, LastSeenAt: now, Metadata: map[string]any{}}
}

func hostname() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "unknown-device"
	}
	return name
}

func DefaultDBPath() string {
	home, _ := os.UserHomeDir()
	if home == "" {
		home = "."
	}
	return filepath.Join(home, ".local", "share", "tokenanalyse", "tokenanalyse.db")
}

func EnsureParent(path string) error {
	dir := filepath.Dir(path)
	if dir == "." || dir == "" {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}

var Version = "dev"

func version() string { return Version }

func ValidateDBPath(path string) error {
	if path == "" {
		return fmt.Errorf("database path is empty")
	}
	return EnsureParent(path)
}
