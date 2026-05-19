package agent

import (
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/axgrid/axup/internal/protocol"
)

// runPgDatabaseTask ensures a PostgreSQL database (and optionally a role with
// LOGIN + full privileges on it) exists. Mirrors the MySQL handler: shells
// out to `psql`, passes the admin password through PGPASSWORD so it stays out
// of argv. Idempotency via pg_database / pg_roles existence checks; encoding
// and owner drift on an existing DB are NOT reconciled.
//
// Postgres has no `CREATE DATABASE IF NOT EXISTS`, so the existence sniff is
// load-bearing — without it we'd hit error 42P04 on every re-run.
func runPgDatabaseTask(ctx *runCtx, t protocol.Task) protocol.Event {
	port := t.DbPort
	if port == 0 {
		port = 5432
	}
	encoding := t.DbEncoding
	if encoding == "" {
		encoding = "UTF8"
	}
	owner := t.DbOwner
	if owner == "" {
		if t.DbUser != "" {
			owner = t.DbUser
		} else {
			owner = t.DbAdminUser
		}
	}

	conn := pgConn{
		host: t.DbHost, port: port,
		user: t.DbAdminUser, pwd: t.DbAdminPassword,
		db: "postgres",
	}

	dbExists, err := conn.exists(fmt.Sprintf(
		"SELECT 1 FROM pg_database WHERE datname = %s;",
		sqlQuotePg(t.DbName)))
	if err != nil {
		return protocol.Event{Status: protocol.StatusError, Message: "check database: " + err.Error()}
	}

	userExists := true
	if t.DbUser != "" {
		userExists, err = conn.exists(fmt.Sprintf(
			"SELECT 1 FROM pg_roles WHERE rolname = %s;",
			sqlQuotePg(t.DbUser)))
		if err != nil {
			return protocol.Event{Status: protocol.StatusError, Message: "check role: " + err.Error()}
		}
	}

	if dbExists && userExists {
		msg := "database already exists"
		if t.DbUser != "" {
			msg = "database and role already exist"
		}
		return protocol.Event{Status: protocol.StatusSkipped, Message: msg}
	}

	if ctx.dryRun {
		var parts []string
		if t.DbUser != "" && !userExists {
			parts = append(parts, fmt.Sprintf("create role %q", t.DbUser))
		}
		if !dbExists {
			parts = append(parts, fmt.Sprintf("create database %q (owner=%q, encoding=%s)", t.DbName, owner, encoding))
		}
		if t.DbUser != "" {
			parts = append(parts, fmt.Sprintf("grant all on database %q to %q", t.DbName, t.DbUser))
		}
		return protocol.Event{
			Status:  protocol.StatusWouldChange,
			Message: "would " + strings.Join(parts, "; "),
		}
	}

	// Postgres won't run CREATE DATABASE inside an explicit transaction
	// block, so we issue each statement standalone rather than batching
	// them in a single -c. psql --single-transaction is similarly off-limits.
	var summary []string

	if t.DbUser != "" && !userExists {
		sql := fmt.Sprintf("CREATE ROLE %s WITH LOGIN PASSWORD %s;",
			pgIdent(t.DbUser), sqlQuotePg(t.DbPassword))
		if stdout, stderr, err := conn.run(sql); err != nil {
			return protocol.Event{
				Status:  protocol.StatusError,
				Stdout:  stdout,
				Stderr:  stderr,
				Message: "create role: " + err.Error(),
			}
		}
		summary = append(summary, "created role "+t.DbUser)
	}

	if !dbExists {
		sql := fmt.Sprintf("CREATE DATABASE %s OWNER %s ENCODING %s;",
			pgIdent(t.DbName), pgIdent(owner), sqlQuotePg(encoding))
		if stdout, stderr, err := conn.run(sql); err != nil {
			return protocol.Event{
				Status:  protocol.StatusError,
				Stdout:  stdout,
				Stderr:  stderr,
				Message: "create database: " + err.Error(),
			}
		}
		summary = append(summary, "created database "+t.DbName)
	}

	if t.DbUser != "" {
		sql := fmt.Sprintf("GRANT ALL PRIVILEGES ON DATABASE %s TO %s;",
			pgIdent(t.DbName), pgIdent(t.DbUser))
		if stdout, stderr, err := conn.run(sql); err != nil {
			return protocol.Event{
				Status:  protocol.StatusError,
				Stdout:  stdout,
				Stderr:  stderr,
				Message: "grant: " + err.Error(),
			}
		}
		summary = append(summary, "granted all on "+t.DbName+" to "+t.DbUser)
	}

	return protocol.Event{
		Status:  protocol.StatusChanged,
		Message: strings.Join(summary, "; "),
	}
}

type pgConn struct {
	host string
	port int
	user string
	pwd  string
	db   string
}

func (c pgConn) base() *exec.Cmd {
	cmd := exec.Command("psql",
		"-h", c.host,
		"-p", strconv.Itoa(c.port),
		"-U", c.user,
		"-d", c.db,
		"-v", "ON_ERROR_STOP=1",
		"-t", "-A", "-X",
	)
	cmd.Env = append(cmd.Environ(),
		"PGPASSWORD="+c.pwd,
		"PGCONNECT_TIMEOUT=10",
	)
	return cmd
}

func (c pgConn) exists(query string) (bool, error) {
	cmd := c.base()
	cmd.Stdin = strings.NewReader(query)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()) != "", nil
}

func (c pgConn) run(sql string) (string, string, error) {
	cmd := c.base()
	cmd.Stdin = strings.NewReader(sql)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// sqlQuotePg wraps a string in single quotes for use as a Postgres string
// literal. PG's standard escape doubles single quotes; we don't emit E''
// strings, so backslashes pass through verbatim (matching default
// standard_conforming_strings = on).
func sqlQuotePg(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// pgIdent double-quotes an identifier. Parse-time validation already
// restricts names to [A-Za-z_][A-Za-z0-9_]{0,62}, so escaping inside is
// unnecessary.
func pgIdent(s string) string {
	return `"` + s + `"`
}
