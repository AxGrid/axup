package agent

import (
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/axgrid/axup/internal/protocol"
)

// runMysqlDatabaseTask ensures a MySQL database (and optionally an app user
// with full privileges on it) exists. We shell out to the `mysql` CLI rather
// than embedding a Go driver to keep the agent binary slim — same trade-off
// as the apt/systemctl/docker handlers. The admin password rides in via
// MYSQL_PWD so it never appears on argv (and therefore not in `ps`).
//
// Idempotency is purely existence-based: information_schema.SCHEMATA for the
// DB, mysql.user for the role. Charset/collation drift on an already-existing
// database is NOT reconciled — this task creates, it doesn't alter.
func runMysqlDatabaseTask(ctx *runCtx, t protocol.Task) protocol.Event {
	port := t.DbPort
	if port == 0 {
		port = 3306
	}
	userHost := t.DbUserHost
	if userHost == "" {
		userHost = "%"
	}
	charset := t.DbCharset
	if charset == "" {
		charset = "utf8mb4"
	}
	collation := t.DbCollation
	if collation == "" {
		collation = "utf8mb4_0900_ai_ci"
	}

	conn := mysqlConn{
		host: t.DbHost, port: port,
		user: t.DbAdminUser, pwd: t.DbAdminPassword,
	}

	dbExists, err := conn.exists(fmt.Sprintf(
		"SELECT SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME=%s;",
		sqlQuoteMy(t.DbName)))
	if err != nil {
		return protocol.Event{Status: protocol.StatusError, Message: "check database: " + err.Error()}
	}

	userExists := true
	if t.DbUser != "" {
		userExists, err = conn.exists(fmt.Sprintf(
			"SELECT 1 FROM mysql.user WHERE user=%s AND host=%s;",
			sqlQuoteMy(t.DbUser), sqlQuoteMy(userHost)))
		if err != nil {
			return protocol.Event{Status: protocol.StatusError, Message: "check user: " + err.Error()}
		}
	}

	if dbExists && userExists {
		// When a user is declared we still want to make sure the grant
		// actually points at the right DB — but recomputing it every run
		// causes unnecessary network chatter and FLUSH PRIVILEGES, so we
		// trust the steady-state. Drift detection is intentionally
		// out-of-scope; rerun with `--check` to spot it.
		msg := "database already exists"
		if t.DbUser != "" {
			msg = "database and user already exist"
		}
		return protocol.Event{Status: protocol.StatusSkipped, Message: msg}
	}

	if ctx.dryRun {
		var parts []string
		if !dbExists {
			parts = append(parts, fmt.Sprintf("create database %q (charset=%s, collation=%s)", t.DbName, charset, collation))
		}
		if t.DbUser != "" && !userExists {
			parts = append(parts, fmt.Sprintf("create user %q@%q", t.DbUser, userHost))
		}
		if t.DbUser != "" {
			parts = append(parts, fmt.Sprintf("grant all on %q.* to %q@%q", t.DbName, t.DbUser, userHost))
		}
		return protocol.Event{
			Status:  protocol.StatusWouldChange,
			Message: "would " + strings.Join(parts, "; "),
		}
	}

	var sql strings.Builder
	var summary []string
	if !dbExists {
		fmt.Fprintf(&sql, "CREATE DATABASE %s CHARACTER SET %s COLLATE %s;\n",
			mysqlIdent(t.DbName), mysqlIdent(charset), mysqlIdent(collation))
		summary = append(summary, "created database "+t.DbName)
	}
	if t.DbUser != "" {
		if !userExists {
			fmt.Fprintf(&sql, "CREATE USER %s@%s IDENTIFIED BY %s;\n",
				sqlQuoteMy(t.DbUser), sqlQuoteMy(userHost), sqlQuoteMy(t.DbPassword))
			summary = append(summary, "created user "+t.DbUser+"@"+userHost)
		}
		// GRANT is idempotent on the server side, but only emit it when we
		// actually changed something so steady-state runs stay quiet.
		fmt.Fprintf(&sql, "GRANT ALL PRIVILEGES ON %s.* TO %s@%s;\n",
			mysqlIdent(t.DbName), sqlQuoteMy(t.DbUser), sqlQuoteMy(userHost))
		sql.WriteString("FLUSH PRIVILEGES;\n")
		summary = append(summary, "granted all on "+t.DbName+".* to "+t.DbUser+"@"+userHost)
	}

	stdout, stderr, err := conn.run(sql.String())
	if err != nil {
		return protocol.Event{
			Status:  protocol.StatusError,
			Stdout:  stdout,
			Stderr:  stderr,
			Message: fmt.Sprintf("mysql apply: %v", err),
		}
	}

	return protocol.Event{
		Status:  protocol.StatusChanged,
		Stdout:  stdout,
		Stderr:  stderr,
		Message: strings.Join(summary, "; "),
	}
}

// mysqlConn is a thin invocation context for shelling out to `mysql`. All
// queries are piped via stdin so neither the SQL body nor the password
// (which lives in MYSQL_PWD) ever land on argv.
type mysqlConn struct {
	host string
	port int
	user string
	pwd  string
}

func (c mysqlConn) base() *exec.Cmd {
	cmd := exec.Command("mysql",
		"-h", c.host,
		"-P", strconv.Itoa(c.port),
		"-u", c.user,
		"--protocol=TCP",
		"-N", "-s", "-r",
		"--default-character-set=utf8mb4",
	)
	cmd.Env = append(cmd.Environ(), "MYSQL_PWD="+c.pwd)
	return cmd
}

// exists runs a SELECT and returns whether any row came back. The query must
// itself include the trailing semicolon — we don't append one.
func (c mysqlConn) exists(query string) (bool, error) {
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

// run executes a body of SQL statements. Returns captured stdout/stderr so
// the agent can surface them in the Event.
func (c mysqlConn) run(sql string) (string, string, error) {
	cmd := c.base()
	cmd.Stdin = strings.NewReader(sql)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// sqlQuoteMy wraps a value in single quotes for use as a MySQL string
// literal, escaping backslashes and single quotes. Identifier names are
// already constrained to [A-Za-z_][A-Za-z0-9_]{0,62} at parse time, but
// passwords / hostnames are free-form so they need real escaping.
func sqlQuoteMy(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('\'')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\'':
			b.WriteString(`\'`)
		case 0:
			b.WriteString(`\0`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// mysqlIdent backtick-quotes an identifier. Because parse.go restricts names
// to [A-Za-z_][A-Za-z0-9_]{0,62}, no escape sequences are needed inside.
func mysqlIdent(s string) string {
	return "`" + s + "`"
}
