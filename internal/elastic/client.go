// Package elastic provides an Elasticsearch client and helpers shared by
// tropobeat's CLI commands.
package elastic

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/spf13/pflag"
)

// Config holds Elasticsearch connection settings. It can be set from CLI
// flags (AddFlags) or unpacked from the beat configuration.
type Config struct {
	Hosts    []string `config:"hosts"`
	Username string   `config:"username"`
	Password string   `config:"password"`
	APIKey   string   `config:"api_key"`
	CACert   string   `config:"ca_cert"`
	Insecure bool     `config:"insecure"`
}

// DefaultHosts is the default Elasticsearch address.
var DefaultHosts = []string{"http://localhost:9200"}

// Environment variables read by AddFlags and ApplyEnv, so that scheduled
// jobs (e.g. cron) don't need credentials on the command line.
const (
	EnvHosts    = "ES_HOSTS" // comma-separated
	EnvUsername = "ES_USERNAME"
	EnvPassword = "ES_PASSWORD"
	EnvAPIKey   = "ES_API_KEY"
)

// AddFlags registers the connection flags on fs, storing values in cfg.
// Hosts and username default to ES_HOSTS and ES_USERNAME when set. Secrets
// are not used as flag defaults (they would appear in --help); call
// ApplyEnv after parsing to read ES_PASSWORD and ES_API_KEY.
func AddFlags(fs *pflag.FlagSet, cfg *Config) {
	hosts := DefaultHosts
	if v := os.Getenv(EnvHosts); v != "" {
		hosts = strings.Split(v, ",")
	}
	fs.StringSliceVar(&cfg.Hosts, "es-host", hosts, "Elasticsearch host URL (repeatable; env "+EnvHosts+")")
	fs.StringVar(&cfg.Username, "es-username", os.Getenv(EnvUsername), "Elasticsearch username (env "+EnvUsername+")")
	fs.StringVar(&cfg.Password, "es-password", "", "Elasticsearch password (env "+EnvPassword+")")
	fs.StringVar(&cfg.APIKey, "es-api-key", "", "Elasticsearch API key, base64 encoded id:api_key (env "+EnvAPIKey+")")
	fs.StringVar(&cfg.CACert, "es-ca-cert", "", "path to a PEM CA certificate to trust")
	fs.BoolVar(&cfg.Insecure, "es-insecure", false, "skip TLS certificate verification")
}

// ApplyEnv fills an empty password or API key from ES_PASSWORD or
// ES_API_KEY.
func (c *Config) ApplyEnv() {
	if c.Password == "" {
		c.Password = os.Getenv(EnvPassword)
	}
	if c.APIKey == "" {
		c.APIKey = os.Getenv(EnvAPIKey)
	}
}

// NewClient creates a client from cfg and verifies the cluster is reachable.
func NewClient(cfg Config) (*elasticsearch.Client, error) {
	if cfg.APIKey != "" && (cfg.Username != "" || cfg.Password != "") {
		return nil, errors.New("use either an API key or username/password, not both")
	}

	esCfg := elasticsearch.Config{
		Addresses: cfg.Hosts,
		Username:  cfg.Username,
		Password:  cfg.Password,
		APIKey:    cfg.APIKey,
	}

	if cfg.CACert != "" {
		cert, err := os.ReadFile(cfg.CACert)
		if err != nil {
			return nil, fmt.Errorf("reading CA certificate: %w", err)
		}
		esCfg.CACert = cert
	}

	if cfg.Insecure {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in via --es-insecure
		esCfg.Transport = transport
	}

	client, err := elasticsearch.NewClient(esCfg)
	if err != nil {
		return nil, err
	}

	res, err := client.Info()
	if err != nil {
		return nil, fmt.Errorf("connecting to Elasticsearch: %w", err)
	}
	defer res.Body.Close()
	if res.IsError() {
		return nil, fmt.Errorf("connecting to Elasticsearch: %s", res.String())
	}

	return client, nil
}
