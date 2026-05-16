package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/axgrid/deploy/internal/inventory"
	"github.com/axgrid/deploy/internal/rulebook"
	"github.com/axgrid/deploy/internal/transport"
)

var (
	logsHost     string
	logsGroup    string
	logsRulebook string
	logsVarsFile string
	logsLines    int
	logsNoFollow bool
	logsList     bool
)

var logsCmd = &cobra.Command{
	Use:   "logs <service>... [flags]",
	Short: "Tail logs from one or more services declared in the rulebook's services: block",
	Long: `Stream logs from a service (or several) across the chosen host(s).

The rulebook's top-level ` + "`services:`" + ` map declares which files belong to
each named service. ` + "`deploy logs`" + ` opens an SSH session per host and runs
` + "`tail -n N -F`" + ` against the resolved paths; lines are forwarded to local
stdout prefixed with ` + "`[host]`" + ` so multi-host output stays attributable.

Examples:

  deploy logs kv crash --host stage-1
  deploy logs --group stage rng billing
  deploy logs --list                     # show declared services
  deploy logs crash --host stage-1 -n 200 --no-follow
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		rb, err := rulebook.Load(logsRulebook, rulebook.LoadOptions{VarsFile: logsVarsFile})
		if err != nil {
			return err
		}
		if logsList {
			return printServicesList(rb)
		}
		if len(args) == 0 {
			return fmt.Errorf("at least one service name is required (try `deploy logs --list`)")
		}
		paths, err := resolveLogPaths(rb, args)
		if err != nil {
			return err
		}
		inv, err := inventory.LoadDir(rb.Dir)
		if err != nil {
			return err
		}
		hosts, err := inv.Resolve(logsHost, logsGroup)
		if err != nil {
			return err
		}

		a, err := resolveAuth()
		if err != nil {
			return err
		}

		// Build the tail command once; `-q` suppresses the per-file
		// "==> path <==" headers (we identify lines by [host] prefix
		// and the service= label inside the JSON-formatted log).
		tailCmd := buildTailCommand(paths, logsLines, !logsNoFollow)

		ctx, cancel := context.WithCancel(cmd.Context())
		defer cancel()
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-sigCh
			cancel()
		}()

		stdoutMu := &sync.Mutex{}
		g, ctx := errgroup.WithContext(ctx)
		for i := range hosts {
			host := hosts[i]
			g.Go(func() error {
				cli, err := transport.Dial(host.Spec, transport.AuthOptions{
					KeyPath:  a.KeyPath,
					Password: a.Password,
				})
				if err != nil {
					return fmt.Errorf("[%s] dial: %w", host.Name, err)
				}
				defer cli.Close()
				pr, pw := io.Pipe()
				done := make(chan error, 1)
				go func() {
					done <- cli.Stream(ctx, tailCmd, pw, pw)
					_ = pw.Close()
				}()
				prefix := fmt.Sprintf("[%s] ", host.Name)
				scan := bufio.NewScanner(pr)
				scan.Buffer(make([]byte, 64*1024), 1024*1024)
				for scan.Scan() {
					stdoutMu.Lock()
					fmt.Print(prefix)
					_, _ = os.Stdout.Write(scan.Bytes())
					fmt.Println()
					stdoutMu.Unlock()
				}
				err = <-done
				// Cancellation is the user's Ctrl-C — not an error.
				if err == context.Canceled || ctx.Err() == context.Canceled {
					return nil
				}
				return err
			})
		}
		if err := g.Wait(); err != nil {
			return err
		}
		return nil
	},
}

func printServicesList(rb *rulebook.Rulebook) error {
	if len(rb.Services) == 0 {
		return fmt.Errorf("rulebook %q has no services declared (add a `services:` block)", rb.Name)
	}
	names := make([]string, 0, len(rb.Services))
	for k := range rb.Services {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Printf("%-20s %s\n", n, rb.Services[n].Logs)
	}
	return nil
}

func resolveLogPaths(rb *rulebook.Rulebook, names []string) ([]string, error) {
	if len(rb.Services) == 0 {
		return nil, fmt.Errorf("rulebook %q has no `services:` block; can't resolve log paths for %v", rb.Name, names)
	}
	var paths []string
	for _, n := range names {
		svc, ok := rb.Services[n]
		if !ok {
			declared := make([]string, 0, len(rb.Services))
			for k := range rb.Services {
				declared = append(declared, k)
			}
			sort.Strings(declared)
			return nil, fmt.Errorf("unknown service %q (declared: %v)", n, declared)
		}
		if len(svc.Logs) == 0 {
			return nil, fmt.Errorf("service %q has no logs declared", n)
		}
		paths = append(paths, svc.Logs...)
	}
	return paths, nil
}

// buildTailCommand assembles a portable `tail` invocation. -F follows
// the file across renames (logrotate-friendly), -q suppresses headers
// when multiple files are tailed together.
func buildTailCommand(paths []string, lines int, follow bool) string {
	flags := fmt.Sprintf("-n %d -q", lines)
	if follow {
		flags += " -F"
	}
	var b strings.Builder
	b.WriteString("tail ")
	b.WriteString(flags)
	for _, p := range paths {
		b.WriteString(" '")
		b.WriteString(p)
		b.WriteString("'")
	}
	return b.String()
}

func init() {
	logsCmd.Flags().StringVar(&logsHost, "host", "", "Target host (user@addr[:port]) or inventory host name")
	logsCmd.Flags().StringVar(&logsGroup, "group", "", "Inventory group name (mutex with --host)")
	logsCmd.Flags().StringVar(&logsRulebook, "rulebook", "rulebook.yaml", "Path to rulebook.yaml")
	logsCmd.Flags().StringVar(&logsVarsFile, "vars", "", "Optional YAML file with vars (templated through service log paths)")
	logsCmd.Flags().IntVarP(&logsLines, "tail", "n", 20, "Initial lines from the end of each log file")
	logsCmd.Flags().BoolVar(&logsNoFollow, "no-follow", false, "Don't stream new lines; print the snapshot and exit")
	logsCmd.Flags().BoolVar(&logsList, "list", false, "Print declared services and exit")
}
