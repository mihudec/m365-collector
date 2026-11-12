package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mihudec/m365-collector/internal/collector"
	"github.com/spf13/cobra"
)

var version = "dev"
var commit = "none"
var date = "unknown"

func command() *cobra.Command {
	var configPath string
	root := &cobra.Command{Use: "m365-collector", Short: "Microsoft 365 audit events as NDJSON for Vector", Version: fmt.Sprintf("%s (%s, %s)", version, commit, date), SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().StringVar(&configPath, "config", "", "YAML configuration file")
	for _, name := range []string{"collect", "run", "subscribe"} {
		name := name
		defaults := collector.Defaults()
		var tenant, client, state, source, graphVersion, wrap string
		var signInTypes []string
		var types []string
		var lookback, overlap, lag, interval, timeout time.Duration
		cmd := &cobra.Command{Use: name, Args: cobra.NoArgs, Short: map[string]string{"collect": "Poll once and exit (Vector scheduled exec)", "run": "Poll continuously (Vector streaming exec)", "subscribe": "Enable configured Activity API subscriptions"}[name], RunE: func(cmd *cobra.Command, _ []string) error {
			c, e := collector.Load(configPath)
			if e != nil {
				return e
			}
			f := cmd.Flags()
			if f.Changed("wrap") {
				c.Wrap = wrap
			}
			if f.Changed("source") {
				c.Source = source
			}
			if f.Changed("graph-version") {
				c.GraphVersion = graphVersion
			}
			if f.Changed("signin-types") {
				c.SignInTypes = signInTypes
			}
			if f.Changed("tenant-id") {
				c.TenantID = tenant
			}
			if f.Changed("client-id") {
				c.ClientID = client
			}
			if f.Changed("state-path") {
				c.StatePath = state
			}
			if f.Changed("content-types") {
				c.ContentTypes = types
			}
			for key, pair := range map[string]struct {
				dst *time.Duration
				v   time.Duration
			}{"lookback": {&c.Lookback, lookback}, "overlap": {&c.Overlap, overlap}, "lag": {&c.Lag, lag}, "interval": {&c.Interval, interval}, "timeout": {&c.Timeout, timeout}} {
				if f.Changed(key) {
					*pair.dst = pair.v
				}
			}
			if e = c.Validate(); e != nil {
				return e
			}
			if name == "subscribe" && c.Source == "graph" {
				return fmt.Errorf("Graph sign-in collection does not require subscriptions; use collect or run")
			}
			secret, e := c.Secret()
			if e != nil {
				return e
			}
			a := collector.NewAPI(c, secret)
			if c.Source == "graph" {
				a = collector.NewGraphAPI(c, secret)
			}
			if name == "subscribe" {
				return a.Subscribe(cmd.Context(), c.ContentTypes)
			}
			s, e := collector.OpenState(c.StatePath)
			if e != nil {
				return e
			}
			defer s.Close()
			for {
				poll := collector.Poll
				if c.Source == "graph" {
					poll = collector.PollGraph
				}
				if e = poll(cmd.Context(), c, a, s, cmd.OutOrStdout(), time.Now()); e != nil {
					return e
				}
				if name == "collect" {
					return nil
				}
				t := time.NewTimer(c.Interval)
				select {
				case <-cmd.Context().Done():
					t.Stop()
					return nil
				case <-t.C:
				}
			}
		}}
		f := cmd.Flags()
		f.StringVar(&wrap, "wrap", "", "Wrap each event in an object under this literal key (empty disables)")
		f.StringVar(&source, "source", defaults.Source, "Source: activity or graph")
		f.StringVar(&graphVersion, "graph-version", defaults.GraphVersion, "Graph API version: v1.0 or beta (opt-in)")
		f.StringSliceVar(&signInTypes, "signin-types", defaults.SignInTypes, "Graph sign-in categories (additional categories require beta)")
		f.StringVar(&tenant, "tenant-id", "", "Tenant UUID")
		f.StringVar(&client, "client-id", "", "Application UUID")
		f.StringVar(&state, "state-path", defaults.StatePath, "SQLite checkpoint path")
		f.StringSliceVar(&types, "content-types", defaults.ContentTypes, "Activity content types")
		f.DurationVar(&lookback, "lookback", defaults.Lookback, "Initial history")
		f.DurationVar(&overlap, "overlap", defaults.Overlap, "Rescan overlap")
		f.DurationVar(&lag, "lag", defaults.Lag, "Availability safety delay")
		f.DurationVar(&interval, "interval", defaults.Interval, "Delay between polls in run mode")
		f.DurationVar(&timeout, "timeout", defaults.Timeout, "HTTP request timeout")
		root.AddCommand(cmd)
	}
	return root
}
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if e := command().ExecuteContext(ctx); e != nil {
		fmt.Fprintln(os.Stderr, "m365-collector:", e)
		os.Exit(1)
	}
}
