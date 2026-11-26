package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
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
	// TaskQueue is the queue the ingest workflow and its activities run on.
	// Every Trelis process serving these Documents must agree on it, or work
	// one of them starts is never picked up.
	TaskQueue string `mapstructure:"task_queue"`
	// ModelTaskQueue is the queue model-calling activities run on, kept
	// separate so ModelActivitiesPerSecond can limit model calls without
	// slowing the rest of ingest. It must differ from TaskQueue: the ingest
	// worker does not register model activities, and the model-call limit
	// would otherwise throttle ordinary ingest.
	ModelTaskQueue string `mapstructure:"model_task_queue"`
	// ModelActivitiesPerSecond is the cap on activity starts per second across
	// every worker on ModelTaskQueue, enforced by the Temporal server. One
	// embed-batch activity sends one embeddings request, apart from the
	// client's own retries on 429 or 5xx inside that attempt, so this is close
	// to a limit on embeddings requests.
	ModelActivitiesPerSecond float64 `mapstructure:"model_activities_per_second"`
}

// HostPort is the frontend gRPC address.
func (t Temporal) HostPort() string {
	return net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
}

// Storage is the object store Source Files are kept in (ADR-0006) and how
// large one may be. Omnisc reads the size limit from MAX_FILE_SIZE_MB.
type Storage struct {
	MaxFileSizeMB int64 `mapstructure:"max_file_size_mb"`
	S3            S3    `mapstructure:"s3"`
}

// S3 configures the S3-compatible bucket (silo, a MinIO fork, in
// development and CI; AWS S3, R2, Ceph or Garage in production).
type S3 struct {
	// Endpoint is the server's base URL, e.g. http://localhost:9000; empty
	// means AWS S3 in Region.
	Endpoint string `mapstructure:"endpoint"`
	Region   string `mapstructure:"region"`
	Bucket   string `mapstructure:"bucket"`
	// AccessKeyID and SecretAccessKey go together; both empty uses the AWS
	// default credential chain. Set the secret through
	// TRELIS_STORAGE_S3_SECRET_ACCESS_KEY.
	AccessKeyID     string `mapstructure:"access_key_id"`
	SecretAccessKey string `mapstructure:"secret_access_key"`
	// ForcePathStyle addresses buckets as {endpoint}/{bucket} even on AWS;
	// every other endpoint is always addressed path-style.
	ForcePathStyle bool `mapstructure:"force_path_style"`
	// CreateBucket creates the bucket on start when it is missing; the
	// development config turns it on. Off (the default), a missing bucket
	// fails /readyz.
	CreateBucket bool `mapstructure:"create_bucket"`
}

func (s S3) validate() []error {
	var errs []error
	if strings.TrimSpace(s.Bucket) == "" {
		errs = append(errs, errors.New("storage.s3.bucket is required"))
	}
	if strings.TrimSpace(s.Region) == "" {
		errs = append(errs, errors.New("storage.s3.region is required"))
	}
	if s.Endpoint != "" {
		u, err := url.Parse(s.Endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("storage.s3.endpoint must be an http(s) URL, got %q", s.Endpoint))
		}
	}
	if (s.AccessKeyID == "") != (s.SecretAccessKey == "") {
		errs = append(errs, errors.New("storage.s3.access_key_id and storage.s3.secret_access_key must be set together"))
	}
	return errs
}

// MaxFileSize is the upload limit in bytes.
func (s Storage) MaxFileSize() int64 {
	return s.MaxFileSizeMB << 20
}
