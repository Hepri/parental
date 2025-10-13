//go:build windows

package session

import (
	"fmt"
	"log"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/Hepri/parental/internal/config"
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
	userenv  = windows.NewLazySystemDLL("userenv.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procLockWorkStation              = user32.NewProc("LockWorkStation")
	procWTSEnumerateSessions         = wtsapi32.NewProc("WTSEnumerateSessionsW")
	procWTSQuerySessionInformation   = wtsapi32.NewProc("WTSQuerySessionInformationW")
	procLogonUser                    = advapi32.NewProc("LogonUserW")
	procCreateProcessAsUser          = advapi32.NewProc("CreateProcessAsUserW")
	procCreateProcessWithLogon       = advapi32.NewProc("CreateProcessWithLogonW")
	procWTSFreeMemory                = wtsapi32.NewProc("WTSFreeMemory")
	procWTSLogonUser                 = wtsapi32.NewProc("WTSLogonUserW")
	procWTSLogoffSession             = wtsapi32.NewProc("WTSLogoffSession")
	procDuplicateTokenEx             = advapi32.NewProc("DuplicateTokenEx")
	procSetTokenInformation          = advapi32.NewProc("SetTokenInformation")
	procCreateEnvironmentBlock       = userenv.NewProc("CreateEnvironmentBlock")
	procDestroyEnvironmentBlock      = userenv.NewProc("DestroyEnvironmentBlock")
	procLoadUserProfile              = userenv.NewProc("LoadUserProfileW")
	procUnloadUserProfile            = userenv.NewProc("UnloadUserProfileW")
	procWTSGetActiveConsoleSessionId = kernel32.NewProc("WTSGetActiveConsoleSessionId")
)

const (
	WTS_CURRENT_SERVER_HANDLE  = 0
	WTSActive                  = 0
	WTSDisconnected            = 1
	WTSConnected               = 2
	WTSConnectState            = 8
	WTSUserName                = 5
	WTSDomainName              = 7
	LOGON32_LOGON_INTERACTIVE  = 2
	LOGON32_PROVIDER_DEFAULT   = 0
	CREATE_UNICODE_ENVIRONMENT = 0x00000400
	TokenSessionId             = 12
)

type profileInfo struct {
	Size        uint32
	Flags       uint32
	UserName    *uint16
	ProfilePath *uint16
	DefaultPath *uint16
	ServerName  *uint16
	PolicyPath  *uint16
	ProfileGuid *uint16
	hProfile    windows.Handle
}

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

		// Poll for session establishment (up to 30s)
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			sessions, err = m.getActiveSessions()
			if err != nil {
				return fmt.Errorf("failed to get updated sessions: %v", err)
			}
			for _, session := range sessions {
				if session.State == WTSActive || session.State == WTSConnected {
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
			if targetSession != nil {
				break
			}
			time.Sleep(1 * time.Second)
		}

		if targetSession == nil {
			return fmt.Errorf("failed to establish session for user %s", username)
		}
	}

	// Change password to temporary simple one for manual login flow
	if err := config.SetUserPassword(username, "123456"); err != nil {
		return fmt.Errorf("failed to set temporary password: %v")
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
				// Logoff this session
				if err := m.logoffSessionByID(session.SessionID); err != nil {
					return err
				}
				// Revert password to configured one
				var configured string
				for _, acc := range m.childAccounts {
					if acc.Username == username {
						configured = acc.Password
						break
					}
				}
				if configured != "" {
					_ = config.SetUserPassword(username, configured)
				}
				return nil
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
					if err := m.logoffSessionByID(session.SessionID); err != nil {
						log.Printf("Failed to lock session for %s: %v", sessionUser, err)
					}
					if account.Password != "" {
						_ = config.SetUserPassword(sessionUser, account.Password)
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
	u, _ := windows.UTF16PtrFromString(account.Username)
	p, _ := windows.UTF16PtrFromString(account.Password)
	app, _ := windows.UTF16PtrFromString("C:\\Windows\\explorer.exe")

	var si windows.StartupInfo
	si.Cb = uint32(unsafe.Sizeof(si))
	desktop, _ := windows.UTF16PtrFromString("winsta0\\default")
	si.Desktop = desktop
	var pi windows.ProcessInformation

	const LOGON_WITH_PROFILE = 1
	const CREATE_UNICODE_ENVIRONMENT = 0x00000400
	const CREATE_NEW_CONSOLE = 0x00000010

	r, _, err := procCreateProcessWithLogon.Call(
		uintptr(unsafe.Pointer(u)),
		0,
		uintptr(unsafe.Pointer(p)),
		LOGON_WITH_PROFILE,
		uintptr(unsafe.Pointer(app)),
		0,
		CREATE_UNICODE_ENVIRONMENT|CREATE_NEW_CONSOLE,
		0, 0,
		uintptr(unsafe.Pointer(&si)),
		uintptr(unsafe.Pointer(&pi)),
	)
	if r == 0 {
		return fmt.Errorf("CreateProcessWithLogonW failed: %v", err)
	}

	windows.CloseHandle(pi.Process)
	windows.CloseHandle(pi.Thread)
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

func (m *Manager) logoffSessionByID(sessionID uint32) error {
	r, _, _ := procWTSLogoffSession.Call(
		WTS_CURRENT_SERVER_HANDLE,
		uintptr(sessionID),
		0,
	)
	if r == 0 {
		return fmt.Errorf("WTSLogoffSession failed")
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
