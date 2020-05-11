package main

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/godbus/dbus"
	"github.com/rs/zerolog/log"
)

const SCHED_OTHER = 0
const SCHED_RR = 2
const SCHED_RESET_ON_FORK = 0x40000000

type schedParam struct {
	schedPriority int32
}

func setRealtimeLimits() error {
	var rlimit syscall.Rlimit
	rlimit.Cur = 1000   // 1ms
	rlimit.Max = 100000 // 100ms

	err := syscall.Setrlimit(unix.RLIMIT_RTTIME, &rlimit)
	if err != nil {
		return fmt.Errorf("setrlimit RLIMIT_RTTIME: %w", err)
	}
	return nil
}

func sys_sched_setscheduler(policy, prio int32) error {
	param := schedParam{prio}

	sysRet, _, err := syscall.Syscall(
		syscall.SYS_SCHED_SETSCHEDULER,
		uintptr(0), // Current process
		uintptr(policy),
		uintptr(unsafe.Pointer(&param)),
	)

	if int(sysRet) == 0 {
		return nil
	} else {
		return err
	}
}

func requestRealtimeDirect(prio int32) error {
	err := sys_sched_setscheduler(SCHED_RR|SCHED_RESET_ON_FORK, prio)
	if err != os.ErrInvalid {
		return err
	}

	// Got EINVAL. There's still a chance that we can set SCHED_RR, and it's just that
	// SCHED_RESET_ON_FORK isn't supported on this machine.

	return sys_sched_setscheduler(SCHED_RR, prio)
}

func requestRealtimeRTKit(tid int, prio int32) error {
	// First, make sure dbus and rtkit actually exist.
	dbusConn, err := dbus.SystemBus()
	if err != nil {
		return fmt.Errorf("DBus connect: %w", err)
	}

	rtkit := dbusConn.Object("org.freedesktop.RealtimeKit1", "/org/freedesktop/RealtimeKit1")

	prop, err := rtkit.GetProperty("org.freedesktop.RealtimeKit1.MaxRealtimePriority")

	if err != nil {
		return fmt.Errorf("query max prio: %w", err)
	}

	maxPrio := prop.Value().(int32)

	// RTKit won't talk to us unless we have SCHED_RESET_ON_FORK.
	err = sys_sched_setscheduler(SCHED_OTHER|SCHED_RESET_ON_FORK, 0)

	if err != nil {
		return fmt.Errorf("set SCHED_RESET_ON_FORK: %w", err)
	}

	if prio > maxPrio {
		prio = maxPrio
	}

	err = rtkit.Call("MakeThreadRealtime", 0, uint64(tid), uint32(prio)).Err
	if err != nil {
		return fmt.Errorf("MakeThreadRealtime: %w", err)
	}

	return nil
}

func attemptRealtime(prio int32) error {
	runtime.LockOSThread()
	tid := syscall.Gettid()

	err := setRealtimeLimits()
	if err != nil {
		goto out_error
	}

	err = requestRealtimeDirect(prio)
	if err != nil {
		err = requestRealtimeRTKit(tid, prio)
	}
	if err != nil {
		goto out_error
	}

	return nil

out_error:
	runtime.UnlockOSThread()
	return err
}

func requestRealtime(name string, prio int32) {
	err := attemptRealtime(prio)
	if err == nil {
		log.Debug().Str("thread", name).Msg("Set thread to realtime")
	} else {
		log.Warn().Str("thread", name).Err(err).Msg("Couldn't get realtime")
	}
}
