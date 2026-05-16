package transport

import (
	"fmt"
	"strings"
)

// Host parses a "[user@]addr[:port]" spec. user defaults to "root"; port to "22".
type Host struct {
	User string
	Addr string
	Port string
}

func ParseHost(spec string) (Host, error) {
	h := Host{User: "root", Port: "22"}
	rest := spec
	if i := strings.Index(rest, "@"); i >= 0 {
		h.User = rest[:i]
		rest = rest[i+1:]
	}
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		h.Addr = rest[:i]
		h.Port = rest[i+1:]
	} else {
		h.Addr = rest
	}
	if h.Addr == "" {
		return Host{}, fmt.Errorf("host spec %q has empty address", spec)
	}
	return h, nil
}

func (h Host) HostPort() string { return h.Addr + ":" + h.Port }
func (h Host) Pretty() string   { return h.User + "@" + h.Addr }
