package remote

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/apex/log"

	"github.com/pyrohost/elytra/src/parser"
)

const (
	SftpAuthPassword  = SftpAuthRequestType("password")
	SftpAuthPublicKey = SftpAuthRequestType("public_key")
)

// A generic type allowing for easy binding use when making requests to API
// endpoints that only expect a singular argument or something that would not
// benefit from being a typed struct.
//
// Inspired by gin.H, same concept.
type d map[string]interface{}

// Same concept as d, but a map of strings, used for querying GET requests.
type q map[string]string

type ClientOption func(c *client)

type Pagination struct {
	CurrentPage uint `json:"current_page"`
	From        uint `json:"from"`
	LastPage    uint `json:"last_page"`
	PerPage     uint `json:"per_page"`
	To          uint `json:"to"`
	Total       uint `json:"total"`
}

// ServerConfigurationResponse holds the server configuration data returned from
// the Panel. When a server process is started, Elytra communicates with the
// Panel to fetch the latest build information as well as get all the details
// needed to parse the given Egg.
//
// This means we do not need to hit Elytra each time part of the server is
// updated, and the Panel serves as the source of truth at all times. This also
// means if a configuration is accidentally wiped on Elytra we can self-recover
// without too much hassle, so long as Elytra is aware of what servers should
// exist on it.
type ServerConfigurationResponse struct {
	Settings             json.RawMessage       `json:"settings"`
	ProcessConfiguration *ProcessConfiguration `json:"process_configuration"`
}

// InstallationScript defines installation script information for a server
// process. This is used when a server is installed for the first time, and when
// a server is marked for re-installation.
type InstallationScript struct {
	ContainerImage string `json:"container_image"`
	Entrypoint     string `json:"entrypoint"`
	Script         string `json:"script"`
}

// RawServerData is a raw response from the API for a server.
type RawServerData struct {
	Uuid                 string          `json:"uuid"`
	Settings             json.RawMessage `json:"settings"`
	ProcessConfiguration json.RawMessage `json:"process_configuration"`
}

type SftpAuthRequestType string

// SftpAuthRequest defines the request details that are passed along to the Panel
// when determining if the credentials provided to Elytra are valid.
type SftpAuthRequest struct {
	Type          SftpAuthRequestType `json:"type"`
	User          string              `json:"username"`
	Pass          string              `json:"password"`
	IP            string              `json:"ip"`
	SessionID     []byte              `json:"session_id"`
	ClientVersion []byte              `json:"client_version"`
}

// SftpAuthResponse is returned by the Panel when a pair of SFTP credentials
// is successfully validated. This will include the specific server that was
// matched as well as the permissions that are assigned to the authenticated
// user for the SFTP subsystem.
type SftpAuthResponse struct {
	Server      string   `json:"server"`
	User        string   `json:"user"`
	Permissions []string `json:"permissions"`
}

type OutputLineMatcher struct {
	// raw string to match against. This may or may not be prefixed with
	// `regex:` which indicates we want to match against the regex expression.
	raw []byte
	reg *regexp.Regexp
}

// Matches determines if the provided byte string matches the given regex or
// raw string provided to the matcher.
func (olm *OutputLineMatcher) Matches(s []byte) bool {
	if olm.reg == nil {
		return bytes.Contains(s, olm.raw)
	}
	return olm.reg.Match(s)
}

// String returns the matcher's raw comparison string.
func (olm *OutputLineMatcher) String() string {
	return string(olm.raw)
}

// UnmarshalJSON unmarshals the startup lines into individual structs for easier
// matching abilities.
func (olm *OutputLineMatcher) UnmarshalJSON(data []byte) error {
	var r string
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}

	olm.raw = []byte(r)
	if bytes.HasPrefix(olm.raw, []byte("regex:")) && len(olm.raw) > 6 {
		r, err := regexp.Compile(strings.TrimPrefix(string(olm.raw), "regex:"))
		if err != nil {
			log.WithField("error", err).WithField("raw", string(olm.raw)).Warn("failed to compile output line marked as being regex")
		}
		olm.reg = r
	}

	return nil
}

// ProcessStopConfiguration defines what is used when stopping an instance.
type ProcessStopConfiguration struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// ProcessConfiguration defines the process configuration for a given server
// instance. This sets what Elytra is looking for to mark a server as done
// starting what to do when stopping, and what changes to make to the
// configuration file for a server.
type ProcessConfiguration struct {
	Startup struct {
		Done            []*OutputLineMatcher `json:"done"`
		UserInteraction []string             `json:"user_interaction"`
		StripAnsi       bool                 `json:"strip_ansi"`
	} `json:"startup"`
	Stop               ProcessStopConfiguration   `json:"stop"`
	ConfigurationFiles []parser.ConfigurationFile `json:"configs"`
}

type BackupRemoteUploadResponse struct {
	Parts    []string `json:"parts"`
	PartSize int64    `json:"part_size"`
}

// S3Credentials represents S3 credentials passed from the panel for rustic backups
type S3Credentials struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token,omitempty"`
	Region          string `json:"region"`
	Endpoint        string `json:"endpoint,omitempty"`
	Bucket          string `json:"bucket"`
	KeyPrefix       string `json:"key_prefix,omitempty"`
	ForcePathStyle  bool   `json:"force_path_style,omitempty"`
	DisableSSL      bool   `json:"disable_ssl,omitempty"`
	CACertPath      string `json:"ca_cert_path,omitempty"`
}

// RusticBackupRequest represents a request to create a rustic backup
type RusticBackupRequest struct {
	BackupType         string         `json:"backup_type"` // "local" or "s3"
	RepositoryPassword string         `json:"repository_password"`
	RepositoryPath     string         `json:"repository_path"` // Panel-provided path for the repository
	S3Credentials      *S3Credentials `json:"s3_credentials,omitempty"`
}

type BackupPart struct {
	ETag       string `json:"etag"`
	PartNumber int    `json:"part_number"`
}

type BackupRequest struct {
	Checksum     string       `json:"checksum"`
	ChecksumType string       `json:"checksum_type"`
	Size         int64        `json:"size"`
	Successful   bool         `json:"successful"`
	Parts        []BackupPart `json:"parts"`
	SnapshotId   string       `json:"snapshot_id,omitempty"`
}

type InstallStatusRequest struct {
	Successful bool `json:"successful"`
	Reinstall  bool `json:"reinstall"`
}

// BackupSizeRecalculation represents backup size updates after deduplication recalculation
type BackupSizeRecalculation struct {
	BackupUuid string `json:"backup_uuid"`
	NewSize    int64  `json:"new_size"`
}

// RecalculatedBackupSizesRequest contains updated backup sizes after repository deduplication
type RecalculatedBackupSizesRequest struct {
	ServerUuid string                    `json:"server_uuid"`
	Backups    []BackupSizeRecalculation `json:"backups"`
}
