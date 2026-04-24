package boot

import "sync"

// ExecMu serializes exec.Cmd calls against drainZombies. PID 1's reap
// loop races with exec.Cmd.Wait: whichever wait4s first wins, and
// exec.Cmd.Wait() returns "waitid: no child processes" when it loses.
//
// Any caller that runs a short-lived subprocess via os/exec on the
// capsule side (iptables, ip, mkfs, runc CLI) must hold ExecMu for the
// lifetime of its cmd.Run()/cmd.CombinedOutput() to keep the reaper
// out of the way. drainZombies takes the lock before calling wait4,
// so it waits until all in-flight exec'd children have been collected
// by their owners.
//
// Not needed for container task processes (containerd owns those) or
// the Firecracker VMM subprocess (its lifecycle is via fc.Machine, not
// raw exec.Cmd).
var ExecMu sync.Mutex
