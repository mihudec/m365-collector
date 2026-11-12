package collector

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"filippo.io/age"
	"go.yaml.in/yaml/v3"
)

type Config struct {
	TenantID     string        `yaml:"tenant_id"`
	ClientID     string        `yaml:"client_id"`
	ClientSecret string        `yaml:"-"`
	SecretFile   string        `yaml:"secret_file"`
	IdentityFile string        `yaml:"identity_file"`
	StatePath    string        `yaml:"state_path"`
	ContentTypes []string      `yaml:"content_types"`
	Lookback     time.Duration `yaml:"lookback"`
	Overlap      time.Duration `yaml:"overlap"`
	Lag          time.Duration `yaml:"lag"`
	Interval     time.Duration `yaml:"interval"`
	Timeout      time.Duration `yaml:"timeout"`
}

func Defaults() Config {
	return Config{StatePath: "state/collector.db", ContentTypes: []string{"Audit.AzureActiveDirectory", "Audit.Exchange", "Audit.SharePoint", "Audit.General"}, Lookback: 24 * time.Hour, Overlap: time.Hour, Lag: 5 * time.Minute, Interval: 5 * time.Minute, Timeout: 60 * time.Second}
}

func Load(path string) (Config, error) {
	c := Defaults()
	if path != "" {
		f, e := os.Open(path)
		if e != nil {
			return c, e
		}
		defer f.Close()
		d := yaml.NewDecoder(f)
		d.KnownFields(true)
		if e = d.Decode(&c); e != nil {
			return c, e
		}
		var extra any
		if e = d.Decode(&extra); e != io.EOF {
			return c, errors.New("config must contain one YAML document")
		}
	}
	for key, dst := range map[string]*string{"TENANT_ID": &c.TenantID, "CLIENT_ID": &c.ClientID, "CLIENT_SECRET": &c.ClientSecret, "SECRET_FILE": &c.SecretFile, "AGE_IDENTITY_FILE": &c.IdentityFile, "STATE_PATH": &c.StatePath} {
		if v, ok := os.LookupEnv("M365_" + key); ok {
			*dst = v
		}
	}
	return c, nil
}
func (c Config) Validate() error {
	for _, id := range []string{c.TenantID, c.ClientID} {
		if len(id) != 36 || strings.ContainsAny(id, "/?# ") {
			return errors.New("tenant_id and client_id must be UUIDs")
		}
		for i, r := range id {
			if i == 8 || i == 13 || i == 18 || i == 23 {
				if r != '-' {
					return errors.New("invalid UUID")
				}
			} else if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
				return errors.New("invalid UUID")
			}
		}
	}
	if c.StatePath == "" || c.Timeout <= 0 || c.Interval <= 0 || c.Lookback <= 0 || c.Lookback > 6*24*time.Hour || c.Overlap < 0 || c.Overlap > 24*time.Hour || c.Lag < 0 || c.Lag > 24*time.Hour {
		return errors.New("invalid state path or durations (lookback <= 144h, overlap/lag <= 24h)")
	}
	if len(c.ContentTypes) == 0 {
		return errors.New("content_types must not be empty")
	}
	seen := map[string]bool{}
	for _, t := range c.ContentTypes {
		switch t {
		case "Audit.AzureActiveDirectory", "Audit.Exchange", "Audit.SharePoint", "Audit.General", "DLP.All":
		default:
			return fmt.Errorf("unsupported content type %q", t)
		}
		if seen[t] {
			return errors.New("duplicate content type")
		}
		seen[t] = true
	}
	return nil
}
func (c Config) Secret() (string, error) {
	if c.ClientSecret != "" {
		return c.ClientSecret, nil
	}
	if c.SecretFile == "" || c.IdentityFile == "" {
		return "", errors.New("set M365_CLIENT_SECRET or secret_file and identity_file")
	}
	identity, e := os.Open(c.IdentityFile)
	if e != nil {
		return "", e
	}
	defer identity.Close()
	ids, e := age.ParseIdentities(identity)
	if e != nil {
		return "", errors.New("invalid age identity file")
	}
	f, e := os.Open(c.SecretFile)
	if e != nil {
		return "", e
	}
	defer f.Close()
	r, e := age.Decrypt(f, ids...)
	if e != nil {
		return "", errors.New("cannot decrypt age secret")
	}
	b, e := io.ReadAll(io.LimitReader(r, 65537))
	if e != nil {
		return "", errors.New("cannot read age secret")
	}
	if len(b) > 65536 || strings.TrimSpace(string(b)) == "" {
		return "", errors.New("invalid secret size")
	}
	return strings.TrimSpace(string(b)), nil
}
