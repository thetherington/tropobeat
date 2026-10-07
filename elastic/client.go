// Package elastic provides an Elasticsearch client and helpers shared by
// tropobeat's CLI commands.
package elastic

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"os"

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

// AddFlags registers the connection flags on fs, storing values in cfg.
func AddFlags(fs *pflag.FlagSet, cfg *Config) {
	fs.StringSliceVar(&cfg.Hosts, "es-host", DefaultHosts, "Elasticsearch host URL (repeatable)")
	fs.StringVar(&cfg.Username, "es-username", "", "Elasticsearch username")
	fs.StringVar(&cfg.Password, "es-password", "", "Elasticsearch password")
	fs.StringVar(&cfg.APIKey, "es-api-key", "", "Elasticsearch API key (base64 encoded id:api_key)")
	fs.StringVar(&cfg.CACert, "es-ca-cert", "", "path to a PEM CA certificate to trust")
	fs.BoolVar(&cfg.Insecure, "es-insecure", false, "skip TLS certificate verification")
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
