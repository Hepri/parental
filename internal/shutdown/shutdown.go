//go:build windows

package shutdown

import (
	"fmt"
	"log"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	advapi32                     = windows.NewLazySystemDLL("advapi32.dll")
	procInitiateSystemShutdownEx = advapi32.NewProc("InitiateSystemShutdownExW")
)

const (
	SHUTDOWN_FORCE_OTHERS        = 0x00000001
	SHUTDOWN_FORCE_SELF          = 0x00000002
	SHUTDOWN_RESTART             = 0x00000004
	SHUTDOWN_POWEROFF            = 0x00000008
	SHUTDOWN_NOREBOOT            = 0x00000010
	SHUTDOWN_GRACE_OVERRIDE      = 0x00000020
	SHUTDOWN_INSTALL_UPDATES     = 0x00000040
	SHUTDOWN_RESTARTAPPS         = 0x00000080
	SHUTDOWN_SKIP_SVC_POPUP      = 0x00000100
	SHUTDOWN_HYBRID              = 0x00000200
	SHUTDOWN_RESTART_BOOTOPTIONS = 0x00000400
)

type ShutdownManager struct {
	scheduledTime *time.Time
	cancelled     bool
}

func NewShutdownManager() *ShutdownManager {
	return &ShutdownManager{}
}

func (sm *ShutdownManager) ScheduleShutdown(delayMinutes int) error {
	if delayMinutes < 0 {
		return fmt.Errorf("delay cannot be negative")
	}

	delaySeconds := int32(delayMinutes * 60)
	message := fmt.Sprintf("Computer will shutdown in %d minutes. This shutdown was initiated by Parental Control Bot.", delayMinutes)

	messagePtr, err := windows.UTF16PtrFromString(message)
	if err != nil {
		return fmt.Errorf("failed to convert message to UTF16: %v", err)
	}

	// Schedule shutdown
	ret, _, _ := procInitiateSystemShutdownEx.Call(
		0, // Local computer
		uintptr(unsafe.Pointer(messagePtr)),
		uintptr(delaySeconds),
		SHUTDOWN_FORCE_OTHERS|SHUTDOWN_GRACE_OVERRIDE, // Force close applications
		0, // Don't restart
	)

	if ret == 0 {
		return fmt.Errorf("InitiateSystemShutdownEx failed")
	}

	shutdownTime := time.Now().Add(time.Duration(delayMinutes) * time.Minute)
	sm.scheduledTime = &shutdownTime
	sm.cancelled = false

	log.Printf("Shutdown scheduled for %d minutes from now", delayMinutes)
	return nil
}

func (sm *ShutdownManager) ShutdownNow() error {
	return sm.ScheduleShutdown(0)
}

func (sm *ShutdownManager) CancelShutdown() error {
	// Cancel scheduled shutdown
	ret, _, _ := procInitiateSystemShutdownEx.Call(
		0, // Local computer
		0, // No message
		0, // No delay
		SHUTDOWN_FORCE_OTHERS|SHUTDOWN_GRACE_OVERRIDE, // Force close applications
		0, // Don't restart
	)

	if ret == 0 {
		return fmt.Errorf("Failed to cancel shutdown")
	}

	sm.scheduledTime = nil
	sm.cancelled = true

	log.Println("Shutdown cancelled")
	return nil
}

func (sm *ShutdownManager) GetScheduledTime() *time.Time {
	return sm.scheduledTime
}

func (sm *ShutdownManager) IsShutdownScheduled() bool {
	return sm.scheduledTime != nil && !sm.cancelled
}
