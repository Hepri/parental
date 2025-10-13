//go:build windows

package session

import (
	"fmt"
	"log"
	"sync"
	"time"
	"unsafe"

	"github.com/Hepri/parental/internal/config"
	"golang.org/x/sys/windows"
)

type ActiveSession struct {
	Username  string
	StartTime time.Time
	Duration  time.Duration
	IsActive  bool
}

type Manager struct {
	childAccounts  []config.ChildAccount
	activeSessions map[string]*ActiveSession
	mutex          sync.RWMutex
}

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	wtsapi32 = windows.NewLazySystemDLL("wtsapi32.dll")
	advapi32 = windows.NewLazySystemDLL("advapi32.dll")

	procLockWorkStation            = user32.NewProc("LockWorkStation")
	procWTSEnumerateSessions       = wtsapi32.NewProc("WTSEnumerateSessionsW")
	procWTSQuerySessionInformation = wtsapi32.NewProc("WTSQuerySessionInformationW")
	procLogonUser                  = advapi32.NewProc("LogonUserW")
	procCreateProcessAsUser        = advapi32.NewProc("CreateProcessAsUserW")
	procWTSFreeMemory              = wtsapi32.NewProc("WTSFreeMemory")
)

const (
	WTS_CURRENT_SERVER_HANDLE = 0
	WTSActive                 = 0
	WTSDisconnected           = 1
	WTSConnected              = 2
	WTSConnectState           = 8
	WTSUserName               = 5
	WTSDomainName             = 7
	LOGON32_LOGON_INTERACTIVE = 2
	LOGON32_PROVIDER_DEFAULT  = 0
)

type WTS_SESSION_INFO struct {
	SessionID      uint32
	WinStationName *uint16
	State          uint32
}

func NewManager(childAccounts []config.ChildAccount) (*Manager, error) {
	return &Manager{
		childAccounts:  childAccounts,
		activeSessions: make(map[string]*ActiveSession),
	}, nil
}

func (m *Manager) GrantAccess(username string, duration time.Duration) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	// Find child account
	var account *config.ChildAccount
	for _, acc := range m.childAccounts {
		if acc.Username == username {
			account = &acc
			break
		}
	}

	if account == nil {
		return fmt.Errorf("child account %s not found", username)
	}

	// Check if user is already logged in
	sessions, err := m.getActiveSessions()
	if err != nil {
		return fmt.Errorf("failed to get active sessions: %v", err)
	}

	var targetSession *WTS_SESSION_INFO
	for _, session := range sessions {
		if session.State == WTSActive {
			// Get username for this session
			sessionUser, err := m.getSessionUsername(session.SessionID)
			if err != nil {
				continue
			}
			if sessionUser == username {
				targetSession = &session
				break
			}
		}
	}

	if targetSession == nil {
		// User is not logged in, need to log them in
		if err := m.logInUser(account); err != nil {
			return fmt.Errorf("failed to log in user %s: %v", username, err)
		}

		// Wait a moment for session to be established
		time.Sleep(2 * time.Second)

		// Get the new session
		sessions, err = m.getActiveSessions()
		if err != nil {
			return fmt.Errorf("failed to get updated sessions: %v", err)
		}

		for _, session := range sessions {
			if session.State == WTSActive {
				sessionUser, err := m.getSessionUsername(session.SessionID)
				if err != nil {
					continue
				}
				if sessionUser == username {
					targetSession = &session
					break
				}
			}
		}

		if targetSession == nil {
			return fmt.Errorf("failed to establish session for user %s", username)
		}
	}

	// Create active session record
	m.activeSessions[username] = &ActiveSession{
		Username:  username,
		StartTime: time.Now(),
		Duration:  duration,
		IsActive:  true,
	}

	log.Printf("Granted access to user %s for %v", username, duration)
	return nil
}

func (m *Manager) LockSession(username string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	// Remove from active sessions
	if session, exists := m.activeSessions[username]; exists {
		session.IsActive = false
		delete(m.activeSessions, username)
	}

	// Get active sessions to find the user's session
	sessions, err := m.getActiveSessions()
	if err != nil {
		return fmt.Errorf("failed to get active sessions: %v", err)
	}

	for _, session := range sessions {
		if session.State == WTSActive {
			sessionUser, err := m.getSessionUsername(session.SessionID)
			if err != nil {
				continue
			}
			if sessionUser == username {
				// Lock this session
				return m.lockSessionByID(session.SessionID)
			}
		}
	}

	return fmt.Errorf("user %s session not found", username)
}

func (m *Manager) LockAllSessions() error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	// Clear all active sessions
	for username, session := range m.activeSessions {
		session.IsActive = false
		log.Printf("Locked session for user %s", username)
	}
	m.activeSessions = make(map[string]*ActiveSession)

	// Lock all child account sessions
	sessions, err := m.getActiveSessions()
	if err != nil {
		return fmt.Errorf("failed to get active sessions: %v", err)
	}

	for _, session := range sessions {
		if session.State == WTSActive {
			sessionUser, err := m.getSessionUsername(session.SessionID)
			if err != nil {
				continue
			}

			// Check if this is a child account
			for _, account := range m.childAccounts {
				if account.Username == sessionUser {
					if err := m.lockSessionByID(session.SessionID); err != nil {
						log.Printf("Failed to lock session for %s: %v", sessionUser, err)
					}
					break
				}
			}
		}
	}

	return nil
}

func (m *Manager) GetActiveSessions() map[string]*ActiveSession {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	result := make(map[string]*ActiveSession)
	for username, session := range m.activeSessions {
		if session.IsActive {
			result[username] = session
		}
	}

	return result
}

func (m *Manager) GetExpiredSessions() []*ActiveSession {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	var expired []*ActiveSession
	now := time.Now()

	for _, session := range m.activeSessions {
		if session.IsActive && now.Sub(session.StartTime) >= session.Duration {
			expired = append(expired, session)
		}
	}

	return expired
}

func (m *Manager) getActiveSessions() ([]WTS_SESSION_INFO, error) {
	var sessionInfo *WTS_SESSION_INFO
	var count uint32

	ret, _, _ := procWTSEnumerateSessions.Call(
		WTS_CURRENT_SERVER_HANDLE,
		0, // Reserved
		1, // Version
		uintptr(unsafe.Pointer(&sessionInfo)),
		uintptr(unsafe.Pointer(&count)),
	)

	if ret == 0 {
		return nil, fmt.Errorf("WTSEnumerateSessions failed")
	}

	defer procWTSFreeMemory.Call(uintptr(unsafe.Pointer(sessionInfo)))

	sessions := make([]WTS_SESSION_INFO, count)
	for i := uint32(0); i < count; i++ {
		sessions[i] = *(*WTS_SESSION_INFO)(unsafe.Pointer(uintptr(unsafe.Pointer(sessionInfo)) + uintptr(i)*unsafe.Sizeof(*sessionInfo)))
	}

	return sessions, nil
}

func (m *Manager) getSessionUsername(sessionID uint32) (string, error) {
	var buffer *uint16
	var bytesReturned uint32

	ret, _, _ := procWTSQuerySessionInformation.Call(
		WTS_CURRENT_SERVER_HANDLE,
		uintptr(sessionID),
		WTSUserName,
		uintptr(unsafe.Pointer(&buffer)),
		uintptr(unsafe.Pointer(&bytesReturned)),
	)

	if ret == 0 {
		return "", fmt.Errorf("WTSQuerySessionInformation failed")
	}

	defer procWTSFreeMemory.Call(uintptr(unsafe.Pointer(buffer)))

	return windows.UTF16PtrToString(buffer), nil
}

func (m *Manager) logInUser(account *config.ChildAccount) error {
	// This is a simplified implementation
	// In a real implementation, you would need to:
	// 1. Use LogonUser to authenticate
	// 2. Use CreateProcessAsUser to start explorer.exe or similar
	// 3. Handle the session creation properly

	username, _ := windows.UTF16PtrFromString(account.Username)
	password, _ := windows.UTF16PtrFromString(account.Password)
	domain, _ := windows.UTF16PtrFromString(".")

	var token windows.Handle
	ret, _, _ := procLogonUser.Call(
		uintptr(unsafe.Pointer(username)),
		uintptr(unsafe.Pointer(domain)),
		uintptr(unsafe.Pointer(password)),
		LOGON32_LOGON_INTERACTIVE,
		LOGON32_PROVIDER_DEFAULT,
		uintptr(unsafe.Pointer(&token)),
	)

	if ret == 0 {
		return fmt.Errorf("LogonUser failed")
	}
	defer windows.CloseHandle(token)

	// Start explorer.exe as the user
	explorerPath, _ := windows.UTF16PtrFromString("C:\\Windows\\explorer.exe")

	var startupInfo windows.StartupInfo
	startupInfo.Cb = uint32(unsafe.Sizeof(startupInfo))

	var processInfo windows.ProcessInformation

	ret, _, _ = procCreateProcessAsUser.Call(
		uintptr(token),
		0, // Application name
		uintptr(unsafe.Pointer(explorerPath)),
		0, // Process attributes
		0, // Thread attributes
		0, // Inherit handles
		0, // Creation flags
		0, // Environment
		0, // Current directory
		uintptr(unsafe.Pointer(&startupInfo)),
		uintptr(unsafe.Pointer(&processInfo)),
	)

	if ret == 0 {
		return fmt.Errorf("CreateProcessAsUser failed")
	}

	windows.CloseHandle(windows.Handle(processInfo.Process))
	windows.CloseHandle(windows.Handle(processInfo.Thread))

	return nil
}

func (m *Manager) lockSessionByID(sessionID uint32) error {
	// For now, we'll use LockWorkStation which locks the current session
	// In a more sophisticated implementation, you would target specific sessions
	ret, _, _ := procLockWorkStation.Call()
	if ret == 0 {
		return fmt.Errorf("LockWorkStation failed")
	}
	return nil
}

func (m *Manager) Cleanup() {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	// Clear all active sessions
	for username, session := range m.activeSessions {
		session.IsActive = false
		log.Printf("Cleaned up session for user %s", username)
	}
	m.activeSessions = make(map[string]*ActiveSession)
}
