package main

// Skills Sync Verb
//
// `harness skills sync` is the only path that creates or fast-forwards a
// serving clone of a declared [skill_repo.<name>] table, at
// $XDG_STATE_HOME/harness/skills/<name>. The daemon never fetches, pulls,
// commits or pushes in a clone (SPEC-0007 REQ "Default-Branch Gate"); this
// command performs the operator's git actions and reports every repo it
// fast-forwarded to the running daemon over the control socket, so the
// daemon reindexes it and merged skills become searchable.
//
// A clone that has diverged, is detached, or is dirty is left exactly as the
// operator left it and reported as not fast-forwarded: the gate keeps serving
// the previous index until the checkout is back on the default branch and
// clean.
//
// Governing: ADR-0030 (grounded skill distillation); SPEC-0007 REQ
// "Default-Branch Gate", REQ "Merge and sync promote", REQ "Skill Repos".
//
// @joestump-agent 09/27/2026 - Added for harness#78.

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/stump-wtf/harness/internal/buildinfo"
	"github.com/stump-wtf/harness/internal/client"
	"github.com/stump-wtf/harness/internal/config"
	"github.com/stump-wtf/harness/internal/skillserve"
)

func newSkillsCmd(g *globalOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:           "skills",
		Short:         "manage skill repo serving clones",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	sync := &cobra.Command{
		Use:           "sync",
		Short:         "clone or fast-forward each skill repo's serving clone and tell the daemon to reindex",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSkillsSync(cmd, g.opts())
		},
	}
	cmd.AddCommand(sync)
	return cmd
}

// runSkillsSync clones or fast-forwards every declared skill repo, then
// reports the fast-forwarded ones to the daemon for reindex.
func runSkillsSync(cmd *cobra.Command, o verbOpts) error {
	cfg, err := config.Load(o.configPath)
	if err != nil {
		return err
	}
	repos := cfg.OrderedSkillRepos()
	if len(repos) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "no skill repos declared in the config; nothing to sync")
		return nil
	}

	var ff []string
	for _, r := range repos {
		res, err := skillserve.SyncRepo(r.Name, r.Remote, skillserve.CloneDir(r.Name))
		switch {
		case err != nil:
			fmt.Fprintf(cmd.ErrOrStderr(), "skills: %s: %v\n", r.Name, err)
		case res.Created:
			fmt.Fprintf(cmd.OutOrStdout(), "skills: %s: cloned from %s (default branch %s)\n", r.Name, r.Remote, res.DefaultBranch)
			ff = append(ff, r.Name)
		case res.FastForwarded:
			fmt.Fprintf(cmd.OutOrStdout(), "skills: %s: fast-forwarded to origin/%s\n", r.Name, res.DefaultBranch)
			ff = append(ff, r.Name)
		default:
			detail := res.Detail
			if detail == "" {
				detail = "already up to date"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "skills: %s: not fast-forwarded (%s)\n", r.Name, detail)
		}
	}

	if len(ff) == 0 {
		return nil
	}
	c, dialErr := client.Dial(o.socket, buildinfo.Version, nil)
	if dialErr != nil {
		// No daemon to tell; the next daemon start reindexes anyway.
		fmt.Fprintln(os.Stderr, "skills: no daemon reachable; it will reindex on its next start")
		return nil
	}
	defer c.Close()
	if _, err := c.SkillsSynced(ff); err != nil {
		return fmt.Errorf("skills: daemon reindex report failed: %w", err)
	}
	return nil
}
