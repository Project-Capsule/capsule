//go:build linux

package firecracker

import (
	"bufio"
	"fmt"
	"os/exec"
	"strings"

	"github.com/geekgonecrazy/capsule/boot"
	capsulev1 "github.com/geekgonecrazy/capsule/models/capsule/v1"
)

// Port mapping for VMs works via iptables DNAT + FORWARD rules that
// route <hostPort> on the capsule to <containerPort> on the VM's bridge
// IP. Each rule is tagged with an iptables comment "capsule-vm:<workload>"
// so teardown can find + remove them by name, even after a capsuled
// restart when we have no in-memory record of what was installed.
//
// This mirrors what CNI's portmap plugin does for containers, except
// we apply it ourselves because VMs don't go through CNI.

// applyPortMappings installs DNAT + FORWARD ACCEPT rules for each entry.
// Rules are tagged with an iptables comment so teardown can match them.
func applyPortMappings(workload, guestIP string, ports []*capsulev1.PortMapping) error {
	comment := portmapComment(workload)
	for _, p := range ports {
		proto := strings.ToLower(p.GetProtocol())
		if proto == "" {
			proto = "tcp"
		}
		host := p.GetHostPort()
		ctr := p.GetContainerPort()
		if host == 0 || ctr == 0 {
			return fmt.Errorf("host_port and container_port are both required (got %d → %d)", host, ctr)
		}

		// DNAT: incoming traffic on the capsule bound for <hostPort> is
		// rewritten to the VM's <containerPort>.
		if err := addIptablesRule("nat", "PREROUTING", []string{
			"-p", proto,
			"--dport", fmt.Sprintf("%d", host),
			"-j", "DNAT",
			"--to-destination", fmt.Sprintf("%s:%d", guestIP, ctr),
			"-m", "comment", "--comment", comment,
		}); err != nil {
			return fmt.Errorf("dnat prerouting: %w", err)
		}

		// Same rule for host-local originated traffic (e.g. curl from the
		// capsule itself to localhost:<hostPort>). PREROUTING doesn't see
		// OUTPUT path.
		if err := addIptablesRule("nat", "OUTPUT", []string{
			"-p", proto,
			"-d", "127.0.0.1",
			"--dport", fmt.Sprintf("%d", host),
			"-j", "DNAT",
			"--to-destination", fmt.Sprintf("%s:%d", guestIP, ctr),
			"-m", "comment", "--comment", comment,
		}); err != nil {
			return fmt.Errorf("dnat output: %w", err)
		}

		// Allow the forwarded traffic through the FORWARD chain in case
		// the capsule's default policy is DROP.
		if err := addIptablesRule("filter", "FORWARD", []string{
			"-p", proto,
			"-d", guestIP,
			"--dport", fmt.Sprintf("%d", ctr),
			"-j", "ACCEPT",
			"-m", "comment", "--comment", comment,
		}); err != nil {
			return fmt.Errorf("forward accept: %w", err)
		}
	}
	return nil
}

// teardownPortMappings removes every iptables rule tagged with this
// workload's comment. Idempotent — fine to call when nothing exists.
func teardownPortMappings(workload string) {
	comment := portmapComment(workload)
	boot.ExecMu.Lock()
	defer boot.ExecMu.Unlock()
	for _, spec := range []struct{ table, chain string }{
		{"nat", "PREROUTING"},
		{"nat", "OUTPUT"},
		{"filter", "FORWARD"},
	} {
		for _, rule := range findRulesWithComment(spec.table, spec.chain, comment) {
			args := append([]string{"-t", spec.table, "-D", spec.chain}, rule...)
			_ = exec.Command("/usr/sbin/iptables", args...).Run()
		}
	}
}

func portmapComment(workload string) string {
	return "capsule-vm:" + workload
}

// addIptablesRule appends the rule only if an identical one (by comment)
// isn't already present — idempotent across reconciler retries. Holds
// boot.ExecMu for the duration to keep the PID-1 reaper out of the way.
func addIptablesRule(table, chain string, body []string) error {
	boot.ExecMu.Lock()
	defer boot.ExecMu.Unlock()
	checkArgs := append([]string{"-t", table, "-C", chain}, body...)
	if err := exec.Command("/usr/sbin/iptables", checkArgs...).Run(); err == nil {
		return nil
	}
	appendArgs := append([]string{"-t", table, "-A", chain}, body...)
	out, err := exec.Command("/usr/sbin/iptables", appendArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables -A %s %s: %w: %s", table, chain, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// findRulesWithComment returns every rule in <table>/<chain> tagged with
// the given iptables comment, as argv suitable for -D. Uses
// `iptables -S` so we can match on the comment exactly.
func findRulesWithComment(table, chain, comment string) [][]string {
	cmd := exec.Command("/usr/sbin/iptables", "-t", table, "-S", chain)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var result [][]string
	needle := `"` + comment + `"`
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, needle) {
			continue
		}
		// iptables -S prints lines like: `-A PREROUTING -p tcp ... -m comment --comment "capsule-vm:foo"`.
		// Strip the leading `-A <chain> ` so the remainder is a rule body
		// that plugs into `-D <chain> ...`.
		fields, err := shellFields(line)
		if err != nil || len(fields) < 2 || fields[0] != "-A" || fields[1] != chain {
			continue
		}
		result = append(result, fields[2:])
	}
	return result
}

// shellFields splits a line while respecting double-quoted sections (to
// preserve a comment that contains spaces). Minimal — iptables comments
// don't contain escape sequences we care about.
func shellFields(s string) ([]string, error) {
	var fields []string
	var cur strings.Builder
	inQuotes := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			inQuotes = !inQuotes
		case c == ' ' && !inQuotes:
			if cur.Len() > 0 {
				fields = append(fields, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		fields = append(fields, cur.String())
	}
	return fields, nil
}
