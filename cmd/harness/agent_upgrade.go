package main

// Agent Upgrade
//
// `harness agent upgrade` re-pins a package-sourced harness to a newer
// commit of the stable's local clone (SPEC-0026 REQ-8): resolve exactly as
// install does (never fetching), diff the candidate against the installed
// pin, rescan with the new-finding marks, run install's gate, then update
// only the @<sha> on each matching source line. Every other key on the
// table is untouched and the prior pin's directory is never deleted —
// rollback is `harness agent install <stable>/<package>@<old-sha> --as
// <name> --replace`, answered from the retained pin with no network.
//
// Governing: ADR-0044 (agent package stables), SPEC-0026 REQ-8, REQ-5 (the
// rescan and its new-finding marks), Error Handling Standards.
//
// @joestump-agent 10/02/2026 - Added for harness#814.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/stump-wtf/harness/internal/agentpkg"
	"github.com/stump-wtf/harness/internal/agentpkg/scan"
	"github.com/stump-wtf/harness/internal/cliui"
	"github.com/stump-wtf/harness/internal/core"
)

type upgradeOpts struct {
	all         bool
	yes         bool
	forceUnsafe bool
}

func newAgentUpgradeCmd(g *globalOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:           "upgrade STABLE/PACKAGE[@VERSION]",
		Short:         "re-pin a package-sourced harness to a newer local-clone commit (diff, rescan, confirm; never fetches)",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			var o upgradeOpts
			o.all, _ = cmd.Flags().GetBool("all")
			o.yes, _ = cmd.Flags().GetBool("yes")
			o.forceUnsafe, _ = cmd.Flags().GetBool("force-unsafe")
			ref := ""
			if len(args) > 0 {
				ref = args[0]
			}
			return runAgentUpgrade(cmd, g.opts(), o, ref)
		},
	}
	cmd.Flags().Bool("all", false, "upgrade every package-sourced harness in the global config")
	cmd.Flags().Bool("yes", false, "skip the ordinary confirmation (never clears a high finding or a write request)")
	cmd.Flags().Bool("force-unsafe", false, "override a high-severity finding (requires an interactive retype)")
	return cmd
}

// upgradeTarget is one package with the harnesses sourcing it.
type upgradeTarget struct {
	src   agentpkg.Source
	names []string
}

// runAgentUpgrade collects the package-sourced harnesses the request names
// (or all of them under --all), then upgrades each package: resolve, diff,
// rescan, gate, re-pin.
func runAgentUpgrade(cmd *cobra.Command, o verbOpts, uo upgradeOpts, ref string) error {
	if uo.all && ref != "" {
		return errors.New("agent: --all and a package reference are mutually exclusive")
	}
	if !uo.all && ref == "" {
		return errors.New("agent: upgrade needs a <stable>/<package>[@<version>] or --all")
	}
	cfg, err := loadGlobalConfig(o.configPath)
	if err != nil {
		return fmt.Errorf("agent: load config: %w", err)
	}

	var version, wantStable, wantPkg string
	if !uo.all {
		var err error
		wantStable, wantPkg, version, err = splitRef(ref)
		if err != nil {
			return err
		}
	}
	targets := map[string]*upgradeTarget{}
	var order []string
	for _, name := range cfg.HarnessOrder {
		h := cfg.Harnesses[name]
		if h.PackageSource == "" {
			continue
		}
		src, err := agentpkg.ParseSource(h.PackageSource)
		if err != nil {
			continue
		}
		if !uo.all && (src.Stable != wantStable || src.Package != wantPkg) {
			continue
		}
		key := src.Stable + "/" + src.Package
		if targets[key] == nil {
			targets[key] = &upgradeTarget{src: agentpkg.Source{Stable: src.Stable, Package: src.Package}}
			order = append(order, key)
		}
		targets[key].names = append(targets[key].names, name)
	}
	if len(order) == 0 {
		if uo.all {
			fmt.Fprintln(cmd.OutOrStdout(), "agent: no package-sourced harnesses in the global config; nothing to upgrade")
			return nil
		}
		return fmt.Errorf("agent: no harness in the global config is installed from %q", ref)
	}

	var firstErr error
	for _, key := range order {
		target := targets[key]
		if err := upgradeOne(cmd, o, uo, cfg, target, version); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "agent: %v\n", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	reloadDaemonIfAny(cmd, o)
	return firstErr
}

// upgradeOne runs the REQ-8 sequence for one package: resolve, diff,
// rescan with new-finding marks, gate, re-pin every harness table.
func upgradeOne(cmd *cobra.Command, o verbOpts, uo upgradeOpts, cfg *core.Config, target *upgradeTarget, version string) error {
	stable, pkg := target.src.Stable, target.src.Package
	if _, exists := cfg.Stables[stable]; !exists {
		return fmt.Errorf("%w: stable %q is not registered", agentpkg.ErrUnknownStable, stable)
	}

	newSrc, err := agentpkg.ResolvePin(stable, pkg, version)
	if err != nil {
		return fmt.Errorf("agent: version %q is not present in stable %q's local clone — run `harness agent stable update %s` first (no fetch is performed on your behalf): %w",
			version, stable, stable, err)
	}

	// Group the harnesses by their current pin: an upgrade shows one diff
	// per distinct installed commit.
	groups := map[string][]string{}
	for _, name := range target.names {
		src, _ := agentpkg.ParseSource(cfg.Harnesses[name].PackageSource)
		groups[src.SHA] = append(groups[src.SHA], name)
	}

	for oldSHA, names := range groups {
		oldSrc := agentpkg.Source{Stable: stable, Package: pkg, SHA: oldSHA}
		if oldSHA == newSrc.SHA {
			for _, name := range names {
				fmt.Fprintf(cmd.OutOrStdout(), "agent: %s: already up to date (@%s)\n", name, oldSHA)
			}
			continue
		}

		newDir := agentpkg.PinDir(newSrc)
		var tmp string
		if _, err := os.Stat(newDir); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("agent: stat %s: %w", newDir, err)
			}
			tmp, err = agentpkg.Materialize(newSrc)
			if err != nil {
				return err
			}
			newDir = tmp
			defer agentpkg.Discard(tmp)
		}

		oldDir := agentpkg.PinDir(oldSrc)
		oldMan, err := agentpkg.LoadManifest(filepath.Join(oldDir, "package.toml"))
		if err != nil {
			return fmt.Errorf("agent: installed pin %s: %w", oldSrc, err)
		}
		newMan, err := agentpkg.LoadManifest(filepath.Join(newDir, "package.toml"))
		if err != nil {
			return fmt.Errorf("agent: candidate pin %s: %w", newSrc, err)
		}

		oldFindings, err := scan.Scan(oldDir, oldMan)
		if err != nil {
			return err
		}
		newFindingsAll, err := scan.Scan(newDir, newMan)
		if err != nil {
			return err
		}
		newFindings := agentpkg.NewSince(oldFindings, newFindingsAll)

		diffLines, err := agentpkg.PinDiff(oldDir, newDir, oldMan, newMan)
		if err != nil {
			return err
		}
		if len(diffLines) > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "diff since @%s:\n", oldSHA)
			for _, l := range diffLines {
				fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", l)
			}
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "diff since @%s: no content changes\n", oldSHA)
		}

		manifestRaw, err := os.ReadFile(filepath.Join(newDir, "package.toml"))
		if err != nil {
			return err
		}
		files, err := agentpkg.BundledFilesIn(newDir)
		if err != nil {
			return err
		}
		agentpkg.RenderReport(cmd.OutOrStdout(), agentpkg.ReportInput{
			Ref:          stable + "/" + pkg,
			ManifestRaw:  manifestRaw,
			Man:          newMan,
			Findings:     newFindingsAll,
			NewFindings:  newFindings,
			BundledFiles: files,
		})
		if hasHigh(newFindingsAll) {
			agentpkg.LogBlocked(stable+"/"+pkg, newFindingsAll)
		}

		gate := agentpkg.DecisionInput{
			Findings:    newFindingsAll,
			Requests:    newMan.Requests,
			Yes:         uo.yes,
			ForceUnsafe: uo.forceUnsafe,
			Interactive: cliui.IsTTY(os.Stdin),
			Ref:         stable + "/" + pkg,
		}
		if err := runGate(cmd, gate); err != nil {
			return err
		}

		// Place the new pin; the prior pin's directory is never deleted.
		if tmp != "" {
			if _, err := agentpkg.Place(newSrc, tmp); err != nil {
				return err
			}
		}

		// Update only the @<sha> on each source line (REQ-8): the #810
		// editor replaces the value in place and touches nothing else.
		ed, err := loadGlobalEditor(o.configPath)
		if err != nil {
			return err
		}
		for _, name := range names {
			if err := ed.SetHarnessKey(name, "source", newSrc.String()); err != nil {
				return fmt.Errorf("agent: update source on [harness.%s]: %w", name, err)
			}
		}
		if err := ed.Save(o.configPath); err != nil {
			return fmt.Errorf("agent: write %s: %w", o.configPath, err)
		}

		if hasHigh(newFindingsAll) && uo.forceUnsafe {
			rec := agentpkg.InstallRecord{
				Source:                    newSrc.String(),
				ScannerVersion:            scan.TableVersion,
				Findings:                  newFindingsAll,
				OverriddenWithForceUnsafe: true,
			}
			if err := agentpkg.WriteInstallRecord(newSrc, rec); err != nil {
				return err
			}
		}

		for _, name := range names {
			fmt.Fprintf(cmd.OutOrStdout(), "agent: upgraded %s: @%s -> @%s (prior pin retained at %s)\n",
				name, oldSHA, newSrc.SHA, oldDir)
		}
	}
	return nil
}
