package sync

import "time"

type Config struct {
	Mode     Mode          `json:"mode"`
	Server   Server        `json:"server,omitempty"`
	WebDAV   WebDAV        `json:"webdav,omitempty"`
	Interval time.Duration `json:"interval"`
}

type Mode string

const (
	ModeDisabled Mode = "disabled"
	ModeServer   Mode = "server"
	ModeWebDAV   Mode = "webdav"
)

type Server struct {
	BaseURL string `json:"baseUrl"`
	Token   string `json:"token"`
}

type WebDAV struct {
	BaseURL    string `json:"baseUrl"`
	Username   string `json:"username"`
	Password   string `json:"password"`
	RemoteRoot string `json:"remoteRoot"`
	Profile    string `json:"profile"`
}

func DefaultConfig() Config {
	return Config{Mode: ModeDisabled, Interval: 5 * time.Minute,
		WebDAV: WebDAV{RemoteRoot: "tokenanalyse", Profile: "default"}}
}
