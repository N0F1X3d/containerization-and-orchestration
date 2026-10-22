// seccompgen компилирует seccomp-профиль в формате Docker (seccomp.json) в сырой BPF,
// который понимает `setpriv --seccomp-filter`. Нужен, чтобы mydocker.sh навешивал
// тот же профиль, что и `docker run --security-opt seccomp=seccomp.json`.
//
//	seccompgen seccomp.json seccomp.bpf
package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	seccomp "github.com/elastic/go-seccomp-bpf"
	"golang.org/x/net/bpf"
)

type profile struct {
	DefaultAction string `json:"defaultAction"`
	Syscalls      []struct {
		Names  []string `json:"names"`
		Action string   `json:"action"`
	} `json:"syscalls"`
}

// action переводит действие Docker (SCMP_ACT_*) в действие библиотеки.
func action(s string) (seccomp.Action, error) {
	var a seccomp.Action
	switch s {
	case "SCMP_ACT_ALLOW":
		return seccomp.ActionAllow, nil
	case "SCMP_ACT_ERRNO":
		return seccomp.ActionErrno, nil // EPERM
	case "SCMP_ACT_KILL", "SCMP_ACT_KILL_THREAD":
		return seccomp.ActionKillThread, nil
	case "SCMP_ACT_KILL_PROCESS":
		return seccomp.ActionKillProcess, nil
	case "SCMP_ACT_LOG":
		return seccomp.ActionLog, nil
	}
	return a, fmt.Errorf("unsupported action %q", s)
}

func run(in, out string) error {
	raw, err := os.ReadFile(in)
	if err != nil {
		return err
	}
	var p profile
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("%s: %w", in, err)
	}

	def, err := action(p.DefaultAction)
	if err != nil {
		return err
	}
	policy := seccomp.Policy{DefaultAction: def}
	for _, g := range p.Syscalls {
		a, err := action(g.Action)
		if err != nil {
			return err
		}
		policy.Syscalls = append(policy.Syscalls, seccomp.SyscallGroup{Names: g.Names, Action: a})
	}

	insns, err := policy.Assemble() // архитектура — текущая (arm64), как и у VM
	if err != nil {
		return err
	}
	prog, err := bpf.Assemble(insns)
	if err != nil {
		return err
	}

	// Формат файла — массив struct sock_filter {u16 code; u8 jt; u8 jf; u32 k}, как ждёт setpriv.
	var b strings.Builder
	for _, ins := range prog {
		var buf [8]byte
		binary.LittleEndian.PutUint16(buf[0:], ins.Op)
		buf[2], buf[3] = ins.Jt, ins.Jf
		binary.LittleEndian.PutUint32(buf[4:], ins.K)
		b.Write(buf[:])
	}
	if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
		return err
	}
	fmt.Printf("%s: %d BPF instructions -> %s\n", in, len(prog), out)
	return nil
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: seccompgen seccomp.json seccomp.bpf")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "seccompgen:", err)
		os.Exit(1)
	}
}
