package config

import (
	"net"
	"net/url"
	"strconv"
)

// Postgres is the Trelis database. Temporal keeps its own databases on the
// same instance, configured on the Temporal server, not here.
type Postgres struct {
	Host     string `mapstructure:"host"`
	Port     int    `mapstructure:"port"`
	DBName   string `mapstructure:"dbname"`
	User     string `mapstructure:"user"`
	Password string `mapstructure:"password"`
	// SSLMode is passed as libpq sslmode: disable, allow, prefer, require,
	// verify-ca or verify-full.
	SSLMode string `mapstructure:"sslmode"`
}

// DSN is the pgx connection URL, with user and password escaped.
func (p Postgres) DSN() string {
	u := url.URL{
		Scheme:   "postgres",
		Host:     net.JoinHostPort(p.Host, strconv.Itoa(p.Port)),
		Path:     "/" + p.DBName,
		RawQuery: url.Values{"sslmode": {p.SSLMode}}.Encode(),
	}
	if p.Password == "" {
		u.User = url.User(p.User)
	} else {
		u.User = url.UserPassword(p.User, p.Password)
	}
	return u.String()
}

// Neo4j is the graph projection store (ADR-0002), reached over Bolt.
type Neo4j struct {
	Host     string `mapstructure:"host"`
	Port     int    `mapstructure:"port"`
	User     string `mapstructure:"user"`
	Password string `mapstructure:"password"`
}

// URI is the Bolt URI the driver dials.
func (n Neo4j) URI() string {
	return "bolt://" + net.JoinHostPort(n.Host, strconv.Itoa(n.Port))
}

// Temporal is the frontend of the Temporal cluster that runs background work (ADR-0005).
type Temporal struct {
	Host      string `mapstructure:"host"`
	Port      int    `mapstructure:"port"`
	Namespace string `mapstructure:"namespace"`
}

// HostPort is the frontend gRPC address.
func (t Temporal) HostPort() string {
	return net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
}

// Storage is where uploaded files are kept (the local FileService) and how
// large one may be. Omnisc reads the same two settings from
// LOCAL_STORAGE_BASE_DIR and MAX_FILE_SIZE_MB.
type Storage struct {
	// LocalDir is created at start if missing.
	LocalDir      string `mapstructure:"local_dir"`
	MaxFileSizeMB int64  `mapstructure:"max_file_size_mb"`
}

// MaxFileSize is the upload limit in bytes.
func (s Storage) MaxFileSize() int64 {
	return s.MaxFileSizeMB << 20
}
