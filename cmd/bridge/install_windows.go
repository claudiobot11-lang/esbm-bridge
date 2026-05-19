// cmdInstall / cmdUninstall — install the Bridge as a native Windows
// service so it auto-starts on boot, restarts on crash, and survives
// user logouts. Also installs Windows Firewall rules so the in-store
// gateway can reach 9071/9080 from the LAN.
//
// All operations require admin rights — the user gets a UAC prompt
// when they run `esbm-bridge install` from a normal cmd window.
//
//go:build windows

package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/claudiobot11-lang/esbm-bridge/internal/winservice"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// firewallRules are the inbound TCP allow rules the Bridge needs so
// the in-store gateway can connect from the LAN. We name them with a
// "ESBM Bridge — " prefix so uninstall finds them all reliably.
var firewallRules = []struct {
	name string
	port int
}{
	{"ESBM Bridge — Cronus (TCP 9071)", 9071},
	{"ESBM Bridge — eStation (TCP 9080)", 9080},
}

func cmdInstall(log *slog.Logger, _ []string) int {
	// 1. Resolve the absolute path of the .exe so the service registers
	//    a stable BinaryPathName. SCM stores the literal path — if the
	//    operator moved the exe later the service would fail to start.
	exePath, err := os.Executable()
	if err != nil {
		log.Error("cannot resolve own executable path", "err", err)
		return 1
	}
	exePath, _ = filepath.Abs(exePath)

	// 2. Connect to the Service Control Manager. Fails (access denied)
	//    when the user isn't running as admin.
	m, err := mgr.Connect()
	if err != nil {
		log.Error("connect to service manager failed (run as Administrator)", "err", err)
		fmt.Fprintln(os.Stderr,
			"Right-click cmd.exe / PowerShell and choose 'Run as administrator',\n"+
				"then re-run: esbm-bridge install")
		return 1
	}
	defer m.Disconnect()

	// 3. Remove any previous registration so re-install always works.
	if s, err := m.OpenService(winservice.ServiceName); err == nil {
		log.Info("removing existing service registration")
		_ = stopService(s)
		_ = s.Delete()
		s.Close()
		// SCM needs a moment to garbage-collect the old entry — give it 1s.
		time.Sleep(1 * time.Second)
	}

	// 4. Create the service. The binary takes one argument: `run`,
	//    which is the same command the operator would invoke
	//    interactively. The service handler in main() flips into
	//    SCM mode when launched without a controlling terminal.
	cfg := mgr.Config{
		ServiceType:      windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:        mgr.StartAutomatic,
		ErrorControl:     mgr.ErrorNormal,
		DisplayName:      winservice.DisplayName,
		Description:      winservice.Description,
		BinaryPathName:   fmt.Sprintf(`"%s" run`, exePath),
		DelayedAutoStart: false, // Boot fast — the gateway is waiting.
	}
	s, err := m.CreateService(winservice.ServiceName, exePath, cfg, "run")
	if err != nil {
		log.Error("create service failed", "err", err)
		return 1
	}
	defer s.Close()

	// 5. Auto-restart policy: if the worker crashes, SCM relaunches it
	//    after 5s, 30s, 5min for the first three failures, then keeps
	//    retrying every 5min. Reset the failure counter daily.
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 5 * time.Minute},
	}, 86400); err != nil {
		log.Warn("could not set recovery actions (non-fatal)", "err", err)
	}

	// 6. Firewall rules. The gateway connects FROM the LAN, so we need
	//    inbound TCP allow on 9071 + 9080. Scoped to this exe to keep
	//    the rule narrow even if someone else listens on the same ports.
	installFirewallRules(log, exePath)

	// 7. Start the service so the gateway can reconnect immediately —
	//    no reboot, no manual "Start" click in services.msc.
	if err := s.Start(); err != nil {
		log.Warn("service installed but failed to start now", "err", err,
			"hint", "open services.msc and start 'ESBM Bridge' manually")
		return 0 // install itself worked
	}

	log.Info("Bridge installed as a Windows service",
		"name", winservice.ServiceName, "exe", exePath)
	fmt.Println()
	fmt.Println("✓ ESBM Bridge is now a Windows service.")
	fmt.Println("  - Starts automatically on boot")
	fmt.Println("  - Restarts automatically if it crashes")
	fmt.Println("  - Firewall rules added for ports 9071 + 9080")
	fmt.Println()
	fmt.Println("Manage via:  services.msc  →  ESBM Bridge")
	fmt.Println("Uninstall:   esbm-bridge uninstall")
	return 0
}

func cmdUninstall(log *slog.Logger, _ []string) int {
	m, err := mgr.Connect()
	if err != nil {
		log.Error("connect to service manager failed (run as Administrator)", "err", err)
		return 1
	}
	defer m.Disconnect()

	s, err := m.OpenService(winservice.ServiceName)
	if err != nil {
		log.Warn("service not registered — nothing to uninstall", "err", err)
	} else {
		log.Info("stopping service")
		if err := stopService(s); err != nil {
			log.Warn("stop service failed (will try delete anyway)", "err", err)
		}
		if err := s.Delete(); err != nil {
			log.Error("delete service failed", "err", err)
			s.Close()
			return 1
		}
		s.Close()
		log.Info("service removed")
	}

	// Firewall cleanup runs unconditionally — leftover rules from a
	// half-failed install would otherwise stay forever.
	removeFirewallRules(log)
	fmt.Println("✓ ESBM Bridge service + firewall rules removed.")
	return 0
}

// stopService asks SCM to stop and waits up to 10s for the worker to
// transition to Stopped. Non-fatal if it times out — the caller is
// usually about to Delete() the service anyway.
func stopService(s *mgr.Service) error {
	status, err := s.Control(svc.Stop)
	if err != nil {
		// Already stopped — fine.
		return nil
	}
	deadline := time.Now().Add(10 * time.Second)
	for status.State != svc.Stopped {
		if time.Now().After(deadline) {
			return fmt.Errorf("stop timeout (current state %d)", status.State)
		}
		time.Sleep(300 * time.Millisecond)
		status, err = s.Query()
		if err != nil {
			return err
		}
	}
	return nil
}

func installFirewallRules(log *slog.Logger, exePath string) {
	// Best-effort: log failures but don't abort the install. If the
	// firewall is already disabled or rule add fails, the operator can
	// still allow the exe interactively the first time Windows asks.
	for _, r := range firewallRules {
		// Remove any prior rule with the same name first so re-install
		// doesn't end up with two identical rules.
		_ = runQuiet("netsh", "advfirewall", "firewall", "delete", "rule",
			"name="+r.name)
		args := []string{
			"advfirewall", "firewall", "add", "rule",
			"name=" + r.name,
			"dir=in", "action=allow",
			"protocol=TCP",
			fmt.Sprintf("localport=%d", r.port),
			"program=" + exePath,
			"profile=any", "enable=yes",
			"description=ESBM Bridge — allow in-store BLE gateway",
		}
		if err := runQuiet("netsh", args...); err != nil {
			log.Warn("firewall rule add failed (non-fatal)",
				"port", r.port, "err", err)
		} else {
			log.Info("firewall rule added", "port", r.port, "name", r.name)
		}
	}
}

func removeFirewallRules(log *slog.Logger) {
	for _, r := range firewallRules {
		if err := runQuiet("netsh", "advfirewall", "firewall", "delete",
			"rule", "name="+r.name); err != nil {
			// Silent — rule may not have been installed.
			log.Debug("firewall rule remove (probably already gone)",
				"port", r.port, "err", err)
		}
	}
}

// runQuiet executes a command and returns its error, discarding output.
// Used for the netsh calls so we don't spam the install log.
func runQuiet(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}
