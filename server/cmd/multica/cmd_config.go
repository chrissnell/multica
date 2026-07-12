package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/multica-ai/multica/server/internal/cli"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Manage configuration for multica",
	RunE:  runConfigShow,
}

var configShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show current CLI configuration",
	RunE:  runConfigShow,
}

var configSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set a CLI configuration value",
	Long:  "Supported keys: server_url, app_url, workspace_id, cf_access_client_id, cf_access_client_secret",
	Args:  exactArgs(2),
	RunE:  runConfigSet,
}

func init() {
	configCmd.AddCommand(configShowCmd)
	configCmd.AddCommand(configSetCmd)
}

func runConfigShow(cmd *cobra.Command, _ []string) error {
	profile := resolveProfile(cmd)
	cfg, err := cli.LoadCLIConfigForProfile(profile)
	if err != nil {
		return err
	}

	path, _ := cli.CLIConfigPathForProfile(profile)
	fmt.Fprintf(os.Stdout, "Config file: %s\n", path)
	if profile != "" {
		fmt.Fprintf(os.Stdout, "Profile:      %s\n", profile)
	}
	fmt.Fprintf(os.Stdout, "server_url:              %s\n", valueOrDefault(cfg.ServerURL, "(not set)"))
	fmt.Fprintf(os.Stdout, "app_url:                 %s\n", valueOrDefault(cfg.AppURL, "(not set)"))
	fmt.Fprintf(os.Stdout, "workspace_id:            %s\n", valueOrDefault(cfg.WorkspaceID, "(not set)"))
	fmt.Fprintf(os.Stdout, "cf_access_client_id:     %s\n", valueOrDefault(cfg.CFAccessClientID, "(not set)"))
	// Print the secret masked. It is stored in a 0600 file on disk, but
	// `multica config` output routinely ends up in bug reports, pastebins,
	// and chat threads — treat it the way we would any other bearer
	// credential and never surface the plaintext here.
	fmt.Fprintf(os.Stdout, "cf_access_client_secret: %s\n", maskSecret(cfg.CFAccessClientSecret))
	return nil
}

// maskSecret renders a stored credential as "(not set)" when empty, "(set,
// <N> chars)" when short enough that revealing character count still doesn't
// leak useful entropy, or the first / last few chars with a middle ellipsis
// for typical-length secrets. Chosen so a support-channel paste of `multica
// config` output confirms whether a secret is configured (and roughly which
// one) without giving anyone reading the paste enough to authenticate.
func maskSecret(v string) string {
	if v == "" {
		return "(not set)"
	}
	if len(v) < 12 {
		return "(set)"
	}
	return v[:4] + "…" + v[len(v)-4:]
}

func runConfigSet(cmd *cobra.Command, args []string) error {
	key, value := args[0], args[1]

	profile := resolveProfile(cmd)
	cfg, err := cli.LoadCLIConfigForProfile(profile)
	if err != nil {
		return err
	}

	switch key {
	case "server_url":
		cfg.ServerURL = value
	case "app_url":
		cfg.AppURL = value
	case "workspace_id":
		cfg.WorkspaceID = value
	case "cf_access_client_id":
		cfg.CFAccessClientID = value
	case "cf_access_client_secret":
		cfg.CFAccessClientSecret = value
	default:
		return fmt.Errorf("unknown config key %q (supported: server_url, app_url, workspace_id, cf_access_client_id, cf_access_client_secret)", key)
	}

	// A half-pair CF Access config is worse than none: setHeaders drops it
	// silently at request time and the origin reverts to CF Access's login
	// redirect, which surfaces as a bogus "server unreachable" downstream.
	// Reject the write here so the user sees the problem at the point of
	// mistake instead of hunting it down later. Don't echo the secret.
	if (cfg.CFAccessClientID == "") != (cfg.CFAccessClientSecret == "") {
		return fmt.Errorf("cf_access_client_id and cf_access_client_secret must both be set (or both unset); Cloudflare Access rejects a request presenting only one header")
	}

	if err := cli.SaveCLIConfigForProfile(cfg, profile); err != nil {
		return err
	}

	// Don't echo secret values back to the terminal (or to the shell
	// history if the user ran the command with `set -x` on).
	if key == "cf_access_client_secret" {
		fmt.Fprintf(os.Stderr, "Set %s (value hidden)\n", key)
	} else {
		fmt.Fprintf(os.Stderr, "Set %s = %s\n", key, value)
	}
	return nil
}

func valueOrDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
